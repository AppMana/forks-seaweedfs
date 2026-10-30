# Audit of Upstream 4.51 (`2dc6a53c6`) and Post-4.51 Master (`d5f405c5d`) Merges

## Executive Summary

This report documents the full audit of the two upstream merge commits on branch `audit-451-agy`:

1. **`2dc6a53c6`** — Merge of upstream tag `4.51` (`153ab668c`) into fork branch `merge/upstream-4.48` (`b9d3b99cb`), merge-base `63d2024b8`.
   - 36 files had textual git conflicts; 53 files in total differ between `2dc6a53c6` and the raw `git merge-tree` auto-merge tree (`74fc0bb0466fda87e5a64057f694e184`).
2. **`d5f405c5d`** — Merge of upstream `master` (`3f954e34d`) into `2dc6a53c6`, merge-base `153ab668c` (`4.51`).
   - 11 files had textual git conflicts; 24 files in total differ between `d5f405c5d` and the raw `git merge-tree` auto-merge tree (`65da3b233ee2356b8f26148f8b4ab1f18ad2c121`).

### Summary of Bugs Found and Fixed on `audit-451-agy`

Every fix below was verified with a regression test that failed (**RED**) before the fix and passed (**GREEN**) after the fix:

| Commit | Area | Bug Found | Fix & Regression Test (RED $\to$ GREEN) |
| :--- | :--- | :--- | :--- |
| `315ff0aff` | `weed/storage` (§1, §3, §5.5) | Fork commit `8cd72fd33` added `v.dataFileSyncLock` to serialize `Append`+`Sync` writes (`writeNeedle2`) against compaction's pre-swap tail copy (`copyDataAndGenerateIndexFile`), preventing a concurrent synced write from being stranded on the pre-swap `.dat` and lost on swap. Upstream `4.51` (`#9150`) added `v.WriteNeedleBlobIfAbsent` (`weed/storage/volume_repair_missing.go:108`) and `v.checkCompactSource` (`weed/storage/volume_vacuum.go:196`), both of which take `v.dataFileAccessLock` without `v.dataFileSyncLock`. Consequently: (1) `WriteNeedleBlobIfAbsent` could append a repaired needle between `writeNeedle2`'s `dataFileAccessLock.Unlock()` and `df.Sync()`, stranding the repaired needle across a concurrent compaction swap; and (2) `checkCompactSource` could read `datSize` before an in-flight `writeNeedle2` finished `df.Sync()`. | Acquired `v.dataFileSyncLock.Lock()` in `WriteNeedleBlobIfAbsent` (`weed/storage/volume_repair_missing.go:108`) and `v.checkCompactSource` (`weed/storage/volume_vacuum.go:196`). Verified with `TestWriteNeedleBlobIfAbsentWaitsForInFlightSyncWrite` (`weed/storage/volume_repair_missing_test.go`) and `TestCheckCompactSourceWaitsForInFlightSyncWrite` (`weed/storage/volume_vacuum_test.go`). |
| `6bec633f3` | `weed/server` (§1, §3) | In `VolumeServer.VolumeCopy` (`weed/server/volume_grpc_copy.go:258`), fork commit `3da4c1881` promoted the target volume's disk headroom reservation via ` reservation.Commit()`. Upstream `4.51` added post-stream source size verification (`checkCopyFileSizes`, lines 281–291) and `MountVolume` error cleanup (lines 296–306) that remove the staged `.dat`/`.idx` files on failure **after** `reservation.Commit()`, when `defer reservation.Release()` is already a no-op—permanently leaking `committedCopyBytes` on the disk location until server restart. | Added `reservation.Uncommit()` (`weed/server/volume_grpc_copy.go:71`) and invoked it whenever `VolumeCopy` removes the staged files and aborts after `reservation.Commit()`. Verified with `TestVolumeCopyRestoresCommittedHeadroomAfterPostStreamFailure` (`weed/server/volume_grpc_copy_verify_test.go`). |
| `d89f56429` | `weed/filer` (§1, §3) | In `Filer.logMetaEvent` (`weed/filer/filer_notify.go:195`), the merge resolution for `2dc6a53c6` placed `f.directoryChanges.noteMetaEvent(ctx, event)` **before** `f.MetaAggregator.RecordLocalEvent(ctx, event)`. When two local mutations occurred within the same nanosecond or under clock skew, `LocalLogRing.AppendEntry` bumped `event.TsNs` monotonically (`lastTsNs + 1`), so `directoryChanges` recorded the stale pre-bump timestamp. `DirectoryPositions` then reported a timestamp lower than the entry's actual log position (`entryVersion.tsNs`), causing `weed mount`'s `awaitDirectoryCurrent` and `NoteDirectoryBasis` to miss the mutation. | Moved `f.directoryChanges.noteMetaEvent(ctx, event)` after `f.MetaAggregator.RecordLocalEvent(ctx, event)` (`weed/filer/filer_notify.go:205`) so `directoryChanges` records the monotonically bumped `event.TsNs`. Verified with `TestNotifyUpdateEventRecordsBumpedLogPositionInDirectoryChanges` (`weed/filer/directory_changes_test.go`). |
| `7dda86092` | `weed/filer` (§5.3) | Two compounding bugs in `weed/filer/reader_at.go`, `reader_cache.go`, and `reader_cache_budget.go` caused severe (up to 64x) S3 GET read amplification under slow-consumer / power-of-2 slab pressure: (1) `mem.AllocationSize` (`weed/util/mem/slot_pool.go:42`) rounds chunk sizes in `(8 MiB, 16 MiB]` (e.g. 10 MiB or 16 MiB Harbor multipart chunks) up to a 16 MiB slab. With `DefaultPrefetchCount = 4` and `-sessionReaderCacheSizeMB=64`, 1 pinned chunk + 4 prefetches required `5 * 16 MiB = 80 MiB > 64 MiB`. While prefetches 1..3 were still in flight (not yet in `b.idle`), prefetch 4's `reserveSelf` found only `chunk0` (pinned!) in `b.idle`, evicted `chunk0` via `pinnedVictim`, and triggered a musical-chairs eviction loop across the window on every 256 KiB `ReadAt` call; (2) in `ChunkReadAt.doReadAt` (`weed/filer/reader_at.go:314`), whenever a sequential `ReadAt` crossed a chunk boundary (`len(tasks) >= 2`, which occurs on every chunk size not divisible by 256 KiB), it called `readChunkSliceAtForParallel` with `stream = nil`, leaving `c.stream` pinned to the finished chunk `tasks[0]` and leaving the newly entered chunk `tasks[len(tasks)-1]` unpinned (`pins == 0`). | (1) Bounded `ReaderCache.MaybeCache` (`weed/filer/reader_cache.go:205`) so `pinnedBytes + windowBytes <= rc.budget.effectiveLimit()`, and guarded `readerCacheBudget.reserveSelf` (`weed/filer/reader_cache_budget.go:141`) so a `pinnedVictim` is never evicted while unpinned reservations are still in flight (`inFlightBytes > 0`); (2) updated `ChunkReadAt.doReadAt` (`weed/filer/reader_at.go:314`) to release `c.stream` after `tasks[0]` and pin `tasks[len(tasks)-1]`. Verified with `TestSequentialSmallReadsDoNotThrashWhenPrefetchWindowExceedsSessionBudget` and `TestSequentialReadsCrossingChunkBoundariesFetchEachChunkOnce` (`weed/filer/reader_at_prefetch_window_test.go`). |
| `9345c4d0f` | `weed/mount` (§2, §3, §5.1) | (1) In `doEnsureVisited` (`weed/mount/meta_cache/meta_cache_init.go:198`), `mc.positionFn(ctx, path)` used the first caller's request `ctx` instead of `buildCtx := context.WithoutCancel(ctx)`, so when the first caller timed out or was interrupted, `positionFn` failed and the shared singleflight build completed without calling `NoteDirectoryBasis(path, basis)`; (2) in `InodeToPath.RemovePathForInode` (`weed/mount/inode_to_path.go:640`), `delete(i.wasCachedDirs, path)` was omitted when upstream `4.51` split `RemovePathForInode` out of `RemovePath`; (3) four FUSE filer RPC paths bypassed `filerReplyTimeout` (`-fuse.metadataRequestTimeout`): `FileHandle.refreshEntryForRead` (`weed/mount/filehandle_read.go:134`), `WFS.syncHardLinkSiblings` (`weed/mount/weedfs_link.go:238`), `WFS.storedRootEntry` (`weed/mount/weedfs.go:730`), and `WFS.doReadDirectory` (`weed/mount/weedfs_dir_read.go:210`). | Fixed `mc.positionFn(buildCtx, path)`, added `delete(i.wasCachedDirs, path)` in `RemovePathForInode`, and bounded `refreshEntryForRead`, `syncHardLinkSiblings`, `storedRootEntry`, and `doReadDirectory` with `filerReplyContext` / `awaitFilerReply`. Verified with `TestEnsureVisitedPositionFnSurvivesOwnerRequestCancel` (`weed/mount/meta_cache/meta_cache_ctx_poison_test.go`), `TestRemovePathForInodeClearsWasCachedDirs` (`weed/mount/inode_to_path_dirstate_test.go`), and `TestFilerRequestsGiveUpOnASilentFiler` (`weed/mount/weedfs_filer_deadline_test.go`). |
| `636a29f59` | `weed/s3api` (§1, §3) | In `CopyObjectPartHandler` (`weed/s3api/s3api_object_handlers_copy.go:1149`), `entry = prefixObjectSource(entry)` was omitted before `classifyCopySourceError(entry, err)` (unlike `CopyObjectHandler` at line 259 and `recheckPolicyForKeyWithLookup` at `s3api_server.go:824`), causing `UploadPartCopy` from a key that also has child keys under its prefix (`IsDirectory=true`, `SeaweedFSPrefixObject="true"`) to fail with `404 NoSuchKey`. | Added `entry = prefixObjectSource(entry)` in `CopyObjectPartHandler` (`weed/s3api/s3api_object_handlers_copy.go:1150`). Verified with `TestCopyObjectPartHandlerPrefixObjectSource` (`weed/s3api/s3api_object_handlers_copy_etag_test.go`). |
| `5673e05da` | `weed/operation`, `weed/util/http` (§5.3, §5.4) | (1) In `uploadStallTimeout` (`weed/operation/upload_content.go:875`), `time.Duration(size/bps) * time.Duration(uploadStallSlowFactor) * time.Second` performed integer division `size/bps` before multiplying by `uploadStallSlowFactor`, truncating up to `8s` of stall budget (and truncating `size/bps` to `0` whenever `size < bps`); (2) in `beginReplicaRead` (`weed/util/http/replica_load.go:77`), the completion callback ignored `err`, so a fast 0-byte HTTP error response (`err != nil`, `bytes == 0`) was recorded as a `1 MiB` read at `<1 ms`, making a failing replica look cheaper than healthy replicas. | (1) Computed `uploadStallTimeout` in floating-point before converting to `time.Duration`; (2) skipped `recordReadCost` in `beginReplicaRead` when `err != nil && bytes <= 0`. Verified with `TestUploadStallTimeoutScalesWithMeasuredRate` (`weed/operation/upload_content_test.go`) and `TestFailedReplicaReadDoesNotLowerLoadScore` (`weed/util/http/replica_load_test.go`). |

---

## 1. Audit of Every Conflict Resolution and Manual Merge Edit

### 1.1 Merge 1: `2dc6a53c6` (`merge/upstream-4.48` + `4.51`)

36 files had git conflicts in `2dc6a53c6`, and 17 additional non-conflicting files were manually edited in the merge commit (53 files total differing from auto-merge tree `74fc0bb0466fda87e5a64057f694e184`):

| File | Conflict / Edit Nature | Verdict |
| :--- | :--- | :--- |
| `weed/command/filer.go` | Combined fork flags (`-DataCenter` wiring, `FilerOption`) with upstream `4.51` flags (`SaveGatewayDataAsChunk` / gateway options). | **SOUND** |
| `weed/command/mount.go`, `weed/command/mount_std.go` | Combined fork `-fuse.metadataRequestTimeout` (`filerReplyTimeout`) and `MetaCache` setup with upstream `4.51` mount options (`IncludeSystemEntries`). | **SOUND** |
| `weed/command/s3.go` | Combined fork `-s3.shareCopyChunks` and bucket config/lock wiring with upstream `4.51` S3 flags. | **SOUND** |
| `weed/command/volume.go` | Combined fork `-inflightUploadDataTimeout` and `ConcurrentUploadLimit` with upstream `4.51` volume flags. | **SOUND** |
| `weed/filer/directory_changes_test.go` | Updated test helper to match upstream `NotifyUpdateEvent` signature. | **SOUND** (extended in `d89f56429`) |
| `weed/filer/filechunk_manifest.go` | Combined fork `ReachableFirst` replica ordering with upstream `4.51` manifest handling. | **SOUND** |
| `weed/filer/filer.go` | Combined fork `directoryChanges` / `LocalLogRing` fields with upstream `4.51` `Filer` struct additions. | **SOUND** |
| `weed/filer/filer_deletion.go` | Combined fork deletion queuing (`fileIdDeletionQueue`) with upstream `4.51` deletion batching. | **SOUND** |
| `weed/filer/filer_notify.go` | Combined fork `f.directoryChanges.noteMetaEvent(ctx, event)` with upstream `4.51` `logMetaEvent`. Placed `noteMetaEvent` **before** `f.MetaAggregator.RecordLocalEvent(ctx, event)`, which bumps `event.TsNs` on same-nanosecond / skewed timestamps. | **BUG — FIXED in `d89f56429`** (`weed/filer/filer_notify.go:205`) |
| `weed/filer/reader_at.go`, `weed/filer/reader_at_test.go` | Combined fork `ChunkReadAt` pin/session budget changes with upstream `4.51` `ReaderAt` changes. | **SOUND** in merge mechanics; see §5.3 and `7dda86092` for boundary-pin and prefetch budget bugs |
| `weed/filer/reader_cache.go` | Combined fork `readerCacheBudget` and `SingleChunkCacher` pin counting with upstream `4.51`. | **SOUND** in merge mechanics; see §5.3 and `7dda86092` |
| `weed/filer/stream.go`, `weed/filer/stream_test.go` | Combined fork `PrepareStreamContentWithPrefetch` / `WriteConfirmed` error surfacing with upstream `4.51`. | **SOUND** |
| `weed/mount/filehandle.go`, `weed/mount/filehandle_read.go`, `weed/mount/filehandle_read_test.go` | Combined fork `entryVersion` and `refreshEntryForRead` with upstream `4.51` handle locking. `refreshEntryForRead` used `withFilerClient` without `filerReplyContext`/`awaitFilerReply`. | **BUG — FIXED in `9345c4d0f`** (`weed/mount/filehandle_read.go:131`) |
| `weed/mount/inode_to_path.go`, `weed/mount/inode_to_path_test.go` | Combined fork `dirState` / `wasCachedDirs` tracking with upstream `4.51` `RemovePathForInode` and hardlink path tracking. `RemovePathForInode` missed `delete(i.wasCachedDirs, path)`. | **BUG — FIXED in `9345c4d0f`** (`weed/mount/inode_to_path.go:640`) |
| `weed/mount/meta_cache/meta_cache.go`, `meta_cache_init.go`, `meta_cache_init_test.go`, `meta_cache_invalidate_test.go`, `meta_cache_subscribe.go`, `meta_cache_subscribe_test.go` | Combined fork `entryVersion`, `sectionBoundsCollector`, `EnsureNameFresh`/`EnsureListingFresh`, and `NoteDirectoryBasis` with upstream `4.51` `IncludeSystemEntries` and `RemovePathForInode`. In `doEnsureVisited` (`meta_cache_init.go:198`), `mc.positionFn(ctx, path)` used caller `ctx` instead of `buildCtx`. | **BUG — FIXED in `9345c4d0f`** (`weed/mount/meta_cache/meta_cache_init.go:198`) |
| `weed/mount/page_writer/upload_pipeline.go`, `upload_pipeline_lock_test.go` | Combined fork `WaitForAsyncFlush` / `WriteError` tracking with upstream `4.51`. | **SOUND** |
| `weed/mount/weedfs.go`, `weed/mount/weedfs_attr.go`, `weed/mount/weedfs_dir_creating.go`, `weed/mount/weedfs_dir_read.go`, `weed/mount/weedfs_dir_read_test.go`, `weed/mount/weedfs_file_CopyFileRange.go`, `weed/mount/weedfs_file_Delete.go`, `weed/mount/weedfs_file_io.go`, `weed/mount/weedfs_file_mkrm.go`, `weed/mount/weedfs_file_sync.go`, `weed/mount/weedfs_link.go`, `weed/mount/weedfs_rename.go` | Combined fork `streamCreateEntry` / `streamUpdateEntry` / `streamDeleteEntry` / `filerReplyTimeout` / `entryVersion` with upstream `4.51` POSIX compliance (`storedRootEntry`, `syncHardLinkSiblings`, `IncludeSystemEntries`). `storedRootEntry`, `syncHardLinkSiblings`, and `doReadDirectory` lacked `filerReplyContext` bounds. | **BUG — FIXED in `9345c4d0f`** (`weed/mount/weedfs.go:681`, `weedfs_link.go:238`, `weedfs_dir_read.go:211`) |
| `weed/operation/upload_chunked.go`, `weed/operation/upload_content.go`, `weed/operation/upload_content_test.go` | Combined fork streaming multipart body (`detachableBodies`), `ProgressWatch`, `StalledServers`, and `fastUploadHosts` stall watchdog with upstream `4.51` `IsReplication` and `Fsync`. | **BUG in `uploadStallTimeout` — FIXED in `5673e05da`** (`weed/operation/upload_content.go:873`) |
| `weed/pb/filer_pb_helper.go` | Combined fork `WithChunksOmitted` / `LookupEntry` with upstream `4.51` helpers. | **SOUND** |
| `weed/s3api/chunk_WriteAt.go`, `s3api_bucket_ComplicatedTest.go`, `s3api_circuit_breaker.go`, `s3api_copy_ verification_test.go`, `s3api_handlers.go`, `s3api_object_handlers.go`, `s3api_object_handlers_complete.go`, `s3api_object_handlers_copy.go`, `s3api_object_handlers_copy_test.go`, `s3api_object_handlers_list.go`, `s3api_object_handlers_post.go`, `s3api_object_handlers_put.go`, `s3api_object_handlers_put_test.go`, `s3api_object_versioning.go`, `s3api_put_routing_test.go`, `s3api_server.go` | Combined fork write-owner routing (`objectWriteLockClient`), conditional CAS writes, `shareCopyChunks`, and `prefixObjectSource` with upstream `4.51` S3 changes. `CopyObjectPartHandler` (`s3api_object_handlers_copy.go:1149`) missed `entry = prefixObjectSource(entry)`. | **BUG — FIXED in `636a29f59`** (`weed/s3api/s3api_object_handlers_copy.go:1150`) |
| `weed/server/filer_grpc_server.go`, `weed/server/filer_grpc_server_sub_meta.go`, `weed/server/filer_grpc_server_traverse_meta.go` | Combined fork `DirectoryPositions`, `StreamMutateEntry`, `WithChunksOmitted`, and `WriteCondition` CAS with upstream `4.51` filer gRPC methods. | **SOUND** |
| `weed/server/volume_grpc_copy.go`, `weed/server/volume_grpc_vacuum.go`, `weed/server/volume_server_handlers_read.go`, `weed/server/volume_server_handlers_write.go` | Combined fork `DiskSpaceReservation.Commit()` in `VolumeCopy`, `inflightUploadDataTimeout`, and `CompactVolume` options with upstream `4.51` `checkCopyFileSizes` and `MountVolume` error cleanup. Failing after `reservation.Commit()` leaked `committedCopyBytes`. | **BUG — FIXED in `6bec633f3`** (`weed/server/volume_grpc_copy.go:71, 286, 301`) |
| `weed/shell/command_volume_fsck.go`, `weed/shell/command_volume_fsck_test.go` | Combined fork stream-to-disk `collectFilerFileIdEntry` and temp-dir cleanup with upstream `4.51` `-verifyNeedle` and `-repairMissingNeedles` (`#9150`). | **SOUND** |
| `weed/storage/disk_location.go`, `weed/storage/store.go`, `weed/storage/volume.go`, `weed/storage/volume_loading.go`, `weed/storage/volume_loading_test.go`, `weed/storage/volume_read_write.go`, `weed/storage/volume_super_block.go`, `weed/storage/volume_test.go`, `weed/storage/volume_vacuum.go`, `weed/storage/volume_vacuum_test.go` | Combined fork `dataFileSyncLock`, non-truncating `.dat` load on tail mismatch, and `DiskSpaceReservation` with upstream `4.51` `checkCompactSource` and `WriteNeedleBlobIfAbsent` (`#9150`). `WriteNeedleBlobIfAbsent` and `checkCompactSource` missed `v.dataFileSyncLock`. | **BUG — FIXED in `315ff0aff`** (`weed/storage/volume_repair_missing.go:108`, `weed/storage/volume_vacuum.go:196`) |

### 1.2 Merge 2: `d5f405c5d` (`2dc6a53c6` + `upstream/master` `3f954e34d`)

11 files had git conflicts in `d5f405c5d`, and 13 additional files were manually edited in the merge commit (24 files total differing from auto-merge tree `65da3b233ee2356b8f26148f8b4ab1f18ad2c121`):

| File | Conflict / Edit Nature | Verdict |
| :--- | :--- | :--- |
| `weed/admin/dash/admin_server.go`, `plugin_ilas.go` | Upstream admin dashboard and plugin updates merged cleanly with fork imports. | **SOUND** |
| `weed/command/mount_std.go` | Preserved fork mount flags alongside upstream post-4.51 mount flag additions. | **SOUND** |
| `weed/filer/stream.go`, `weed/filer/stream_test.go` | Preserved fork `WriteConfirmed` error propagation (`0dd743d2d`) alongside upstream stream updates. | **SOUND** |
| `weed/mount/meta_cache/meta_cache_init.go`, `meta_cache_subscribe.go`, ` weed/mount/weedfs.go`, `weed/mount/weedfs_dir_read.go`, `weed/mount/weedfs_file_mkrm.go` | Combined fork `filerReplyTimeout`, section refresh, and `streamDeleteEntry` with upstream post-4.51 mount changes. | **SOUND** (with `9345c4d0f` fixes applied) |
| `weed/s3api/s3api_bucket_handlers.go`, `s3api_object_handlers.go`, `s3api_object_handlers_copy.go`, `s3api_object_handlers_copy_acl_test.go`, `s3api_object_handlers_delete.go`, `s3api_object_handlers_DeleteMultipleObjects_test.go`, `s3api_object_handlers_head_test.go`, `s3api_object_handlers_list.go`, `s3api_object_handlers_put.go`, `s3api_object_handlers_rename.go`, `s3api_put_routing_test.go`, `s3api_server.go`, `s3api_server_test.go` | Combined fork ACL/ownership preservation (`5faae6510`), conditional headers (`ff53a0807`), `RenameObject` (`a3a6f7f16`), and `UnifiedPostHandler` (`8c0b467e1`) with upstream post-4.51 S3 changes (`#9199`, `#9281`). In `CopyObjectPartHandler` (`s3api_object_handlers_copy.go:1149`), `prefixObjectSource` was still missing. | **BUG — FIXED in `636a29f59`** |
| `weed/storage/volume_checking.go` | Preserved fork non-destructive index check behavior alongside upstream post-4.51 changes. | **SOUND** |

---

## 2. Fork Fixes Audited Across Clean (Non-Conflicting) Merges

We audited all non-merge fork commits in `git log --no-merges 4.48..2dc6a53c6^1` (excluding commits already in `upstream/master`) to verify that no fork fix or test was silently overwritten or bypassed by a clean merge in `2dc6a53c6` or `d5f405c5d`:

- **Storage & Volume Integrity:**
  - `8cd72fd33` (`storage: hold sync write until data file syncs so compaction copies it`): Survived in `writeNeedle2` and `copyDataAndGenerateIndexFile`, but upstream `4.51` added two new callers (`WriteNeedleBlobIfAbsent` and `checkCompactSource`) that bypassed `dataFileSyncLock` — **fixed in `315ff0aff`**.
  - `3da4c1881` (`storage,server: reserve disk headroom across concurrent VolumeCopy and CompactVolume`): Survived in `weed/storage/disk_location.go` and `weed/server/volume_grpc_copy.go`, but upstream `4.51`'s post-stream failure paths leaked the committed reservation — **fixed in `6bec633f3`**.
  - `7df7c5f14` / `a02e96310` (never truncate `.dat` on volume load when `.idx` is shorter than `.dat`): Verified intact in `weed/storage/volume_loading.go:180-226` and tested in `weed/storage/volume_loading_test.go`.
- **Filer & Stream:**
  - `0dd743d2d` (`filer: surface downstream Write error in StreamContent instead of logging as chunk fetch failure`): Verified intact in `weed/filer/stream.go:257-325`.
  - `f452d316b` (`filer: bound SessionReaderCache and evict idle chunks`): Verified intact in `weed/filer/reader_cache.go` and `reader_cache_budget.go`; prefetch window vs budget thrashing and boundary-crossing pin drop fixed in `7dda86092`.
  - `b8d210776` (`filer: track per-directory log positions for mount cache currency`): Survived in `weed/filer/directory_changes.go`, but `logMetaEvent` in `weed/filer/filer_notify.go:195` called `noteMetaEvent` before `RecordLocalEvent` bumped `event.TsNs` — **fixed in `d89f56429`**.
- **Mount / FUSE:**
  - `901f2b3a9` (`mount: bound filer waits with filerReplyTimeout`): Survived on `streamCreateEntry`, `streamUpdateEntry`, `streamDeleteEntry`, `doRename`, and `lookupEntry`, but missed `refreshEntryForRead`, `syncHardLinkSiblings`, `storedRootEntry`, and `doReadDirectory` — **fixed in `9345c4d0f`**.
  - `4e711f020` (`mount: detach EnsureVisited singleflight build from requester context`): Survived in `doEnsureVisited`, except `mc.positionFn(ctx, path)` still used `ctx` instead of `buildCtx` — **fixed in `9345c4d0f`**.
  - `d6202a1d8` (`mount: rebuild expired directory cache on lookup only for directories that were once cached`): Survived in `weed/mount/inode_to_path.go`, except `RemovePathForInode` missed `delete(i.wasCachedDirs, path)` — **fixed in `9345c4d0f`**.
- **S3 API & Operation:**
  - `5faae6510` / `a3a6f7f16` (`s3api: support prefix-object sources in CopyObject and RenameObject`): Survived in `CopyObjectHandler`, `RenameObjectHandler`, and `recheckPolicyForKeyWithLookup`, but missed `CopyObjectPartHandler` — **fixed in `636a29f59`**.
  - `2c8971a04` (`operation: stream multipart upload body without copying payload`): Verified intact in `weed/operation/upload_content.go:569-632`.
  - `68978d1c7` (`operation: add ProgressWatch and StalledServers`): Verified intact in `weed/operation/progress_watch.go` and `stalled_servers.go`.
  - `e829140df` (`util/http: route chunk reads by replica load and reachability`): Verified intact in `weed/util/http/replica_load.go` and `unreachable.go`; 0-byte failed read cost distortion fixed in `5673e05da`.

---

## 3. Semantic Collisions Between Upstream and Fork

1. **`Volume.dataFileSyncLock` vs Upstream `#9150` (`WriteNeedleBlobIfAbsent` and `checkCompactSource`)**:
   - Upstream `#9150` assumed `v.dataFileAccessLock` was the only lock protecting `.dat`/`.idx` appends and compaction tail checks. The fork had split out `v.dataFileSyncLock` so `writeNeedle2` holds `dataFileSyncLock` across `df.Sync()` after releasing `dataFileAccessLock`. Fixed in `315ff0aff`.
2. **`DiskSpaceReservation.Commit()` vs Upstream Post-Stream `VolumeCopy` Checks**:
   - The fork called `reservation.Commit()` as soon as the `.dat`/`.idx` copy stream completed, assuming `VolumeCopy` only mounted the volume afterward. Upstream `4.51` inserted `checkCopyFileSizes` and `MountVolume` rollback file deletion after stream completion, leaking the committed reservation on error. Fixed in `6bec633f3`.
3. **`Filer.directoryChanges.noteMetaEvent` vs `MetaAggregator.RecordLocalEvent` Timestamp Bump**:
   - Upstream's `logMetaEvent` mutated `event` in-place inside `RecordLocalEvent` (`LocalLogRing.AppendEntry` bumps `event.TsNs` when `event.TsNs <= lastTsNs`). Placing `f.directoryChanges.noteMetaEvent` before `RecordLocalEvent` caused `directoryChanges` to record the pre-bump timestamp. Fixed in `d89f56429`.
4. **`InodeToPath.RemovePath` vs Upstream `RemovePathForInode`**:
   - Upstream `4.51` introduced `RemovePathForInode` for inode-checked path removal on metadata invalidation events, duplicating `RemovePath`'s map deletions without the fork's `delete(i.wasCachedDirs, path)`. Fixed in `9345c4d0f`.
5. **`WFS.withFilerClient` / `filerReplyTimeout` vs Upstream `filer_pb.GetEntry` Callers (`storedRootEntry`, `syncHardLinkSiblings`)**:
   - Upstream `4.51` added `storedRootEntry` (`weed/mount/weedfs.go:730`) calling `filer_pb.GetEntry(context.Background(), wfs, ...)` and `syncHardLinkSiblings` (`weed/mount/weedfs_link.go:253`) calling `filer_pb.GetEntry(ctx, wfs, p)` with `metadataFlushTimeout = 30s`. `filer_pb.GetEntry` calls `wfs.WithFilerClient(false, ...)`, which retries internally using `context.Background()` and ignores the caller's `ctx` for per-attempt dial/RPC timeouts unless wrapped in `awaitFilerReply` + `wfs.withFilerClient(waitCtx, ...)`. Fixed in `9345c4d0f`.

---

## 4. Audit of Tests Changed to Pass During the Merges

Every test file modified in `2dc6a53c6` and `d5f405c5d` was inspected:

1. **`weed/filer/directory_changes_test.go`** (`2dc6a53c6`):
   - **Change:** Updated `NotifyUpdateEvent` calls in the test to pass the 5th argument (`fromOtherCluster bool`) added by upstream `4.51`.
   - **Judgment:** **Justified** signature adaptation. (We added `TestNotifyUpdateEventRecordsBumpedLogPositionInDirectoryChanges` in `d89f56429` to test the `logMetaEvent` timestamp-bump ordering.)
2. **`weed/filer/reader_at_test.go` & `weed/filer/stream_test.go`** (`2dc6a53c6`, `d5f405c5d`):
   - **Change:** Merged new fork tests (`TestStreamContentSurfacesDownstreamWriteError`, `TestChunkReadAt...`) alongside upstream tests without weakening assertions.
   - **Judgment:** **Justified**.
3. **`weed/mount/filehandle_read_test.go`, `weed/mount/inode_to_path_test.go`, `weed/mount/weedfs_dir_read_test.go`** (`2dc6a53c6`):
   - **Change:** Adapted test helpers for the `entryVersion` return value from `maybeLoadEntry` / `FindEntry` and upstream's `includeSystemEntries` parameter on `loadDirectoryEntriesDirect`.
   - **Judgment:** **Justified**.
4. **`weed/mount/meta_cache/meta_cache_init_test.go`, `meta_cache_invalidate_test.go`, `meta_cache_subscribe_test.go`** (`2dc6a53c6`):
   - **Change:** Updated `NewMetaCache` calls in tests to pass the new `includeSystemEntries` boolean and `RemovePathForInode` callback signature.
   - **Judgment:** **Justified**.
5. **`weed/operation/upload_content_test.go`** (`2dc6a53c6`):
   - **Change:** Combined fork stall-watchdog and `AssignVolume` timeout tests with upstream's `TestReplicationUploadPreservesUncompressedNeedle` (`#9141`).
   - **Judgment:** **Justified**.
6. **`weed/s3api/s3api_bucket_ComplicatedTest.go`, `s3api_copy_verification_test.go`, `s3api_object_handlers_copy_test.go`, `s3api_object_handlers_put_test.go`, `s3api_put_routing_test.go`, `s3api_object_handlers_copy_acl_test.go`, `s3api_object_handlers_DeleteMultipleObjects_test.go`, `s3api_object_handlers_head_test.go`, `s3api_server_test.go`** (`2dc6a53c6`, `d5f405c5d`):
   - **Change:** Merged fork test suites for routed PUTs, conditional copy headers, and ACL ownership with upstream S3 tests (`#9199`, `#9281`). No assertions were weakened.
   - **Judgment:** **Justified**.
7. **`weed/shell/command_volume_fsck_test.go`** (`2dc6a53c6`):
   - **Change:** Combined fork disk-backed B+tree fsck tests with upstream's `-verifyNeedle` and `-repairMissingNeedles` tests.
   - **Judgment:** **Justified**.
8. **`weed/storage/volume_loading_test.go`, `weed/storage/volume_test.go`, `weed/storage/volume_vacuum_test.go`** (`2dc6a53c6`):
   - **Change:** Preserved fork test `TestVolumeLoadDoesNotTruncateDatFileWhenIdxIsShorter` while integrating upstream's `TestCompactVolumeDetectsSourceModifiedDuringCompaction` (`#9150`).
   - **Judgment:** **Justified**.

---

## 5. Behaviour Under Load

### 5.1 FUSE Request Deadlines (`weed/mount`)

- **Requirement:** Every FUSE request that waits on the filer must give up within `filerReplyTimeout` (`-fuse.metadataRequestTimeout`, default `60s`), because the kernel holds the caller's VFS inode/directory lock (`i_rwsem`) in uninterruptible sleep (`D`-state) until the FUSE daemon replies.
- **Audit Findings:**
  - Mutations (`Create`, `Mknod`, `Mkdir`, `Unlink`, `Rmdir`, `Symlink`, `Link`, `SetAttr`, `CopyFileRange`, `Release`/`Flush` via `streamCreateEntry`, `streamUpdateEntry`, `streamDeleteEntry`, and `doRename`) and `Lookup` (`lookupEntry`) properly bounded their filer waits via `filerReplyContext` and `awaitFilerReply`.
  - **Four gaps were found and fixed in `9345c4d0f`:**
    1. `FileHandle.refreshEntryForRead` (`weed/mount/filehandle_read.go:134`): Called `fh.wfs.withFilerClient(ctx, ...)` without `filerReplyContext` / `awaitFilerReply`, allowing `util.Retry("filer grpc", ...)` to block a `Read` for minutes when a chunk HTTP 404 triggered a handle refresh against a silent filer.
    2. `WFS.syncHardLinkSiblings` (`weed/mount/weedfs_link.go:238`): Used `metadataFlushTimeout` (`30s`) and `filer_pb.GetEntry(ctx, wfs, p)` (which calls `wfs.WithFilerClient` on `context.Background()`), blocking `Link`, `Unlink`, and `SetAttr` on hard-linked files when the filer was silent.
    3. `WFS.storedRootEntry` (`weed/mount/weedfs.go:730`): Called `filer_pb.GetEntry(context.Background(), wfs, ...)` while holding `wfs.storedRootMu`, blocking `GetAttr(root)` and `maybeLoadEntry(root)` indefinitely on a silent filer.
    4. `WFS.doReadDirectory` (`weed/mount/weedfs_dir_read.go:210`): Did not wrap `ctx` in `filerReplyContext(ctx)`, so `ReadDir` / `ReadDirPlus` waited `sectionRefreshTimeout = 30s` on stale sections or indefinitely on `ensureDirectoryVisited` / `readDirectoryDirect` while holding `wfs.holdKernelLock(input.NodeId)`.

### 5.2 Filer Metadata Latency (`weed/filer`, `weed/server`)

- **Audit Findings:**
  - `WithChunksOmitted` (`weed/pb/filer_pb_helper.go`, `weed/server/filer_grpc_server.go`): `ReadDir` and `ReadDirPlus` strip chunk slices when listing cached entries from LevelDB, avoiding per-child `FileChunk` protobuf allocations on wide directories.
  - `DirectoryPositions` (`weed/filer/directory_changes.go`, `weed/server/filer_grpc_server.go`): O(1) in-memory map lookup per requested directory path. With commit `d89f56429`, `directoryChanges` now records the monotonically bumped `event.TsNs` after `MetaAggregator.RecordLocalEvent`, preventing false stale-position mismatches under high mutation rates.
  - `StreamMutateEntry` (`weed/server/filer_grpc_server.go`): Reuses a single bidirectional gRPC stream per mount via `streamMutateMux` (`weed/mount/weedfs_stream_mutate.go`), avoiding per-create/update/delete RPC setup overhead while isolating slow renames with an idle progress timer.

### 5.3 S3 GET Read Amplification (`REVIEW_PROMPT.md` & `slow_proxy.py` Investigation)

We investigated the production incident described in `REVIEW_PROMPT.md` (where `s3_gateway` pods Serving Harbor registry blobs exhibited **3.6x–4.3x steady-state read amplification**, downloading `532 MB/s` from volume servers to serve `124 MB/s` of S3 `GET` traffic with `-cacheCapacityMB=2048` and `-sessionReaderCacheSizeMB=64`) and traced all four questions from `REVIEW_PROMPT.md`:

#### Question 1: Where does the 3.6x–4.3x read amplification come from?

There are **two direct mechanisms in `weed/filer/reader_at.go` + `reader_cache.go` + `reader_cache_budget.go`** (both fixed in commit `7dda86092`) and **three architectural bypasses** when chunks are unaligned, encrypted, or copied via `UploadPartCopy`:

1. **Mechanism A — Power-of-2 Slab Rounding + Prefetch Window Exceeding `-sessionReaderCacheSizeMB=64` (Fixed in `7dda86092`):**
   - `SingleChunkCacher.startCaching` (`weed/filer/reader_cache.go:315`) allocates chunk buffers via `mem.Allocate(int(s.chunkSize))`, and `readerCacheBudget.reserveSelf` (`weed/filer/reader_cache.go:303`) charges `mem.AllocationSize(int(s.chunkSize))`.
   - In `weed/util/mem/slot_pool.go:42-52`, `mem.AllocationSize(size)` rounds every buffer size **up to the next power of 2** (`1, 2, 4, 8, 16, 32, 64 MiB...`).
   - Harbor and S3 multipart uploads commonly produce **10 MiB, 15 MiB, or 16 MiB parts** (or 8 MiB + residual chunks). Any chunk in `(8 MiB, 16 MiB]` is charged **16 MiB** against `sessionBudget`; any chunk in `(16 MiB, 32 MiB]` is charged **32 MiB**.
   - `ChunkReadAt` defaults to `DefaultPrefetchCount = 4` (`weed/filer/reader_at.go:40`), so `MaybeCache` requests **4 lookahead chunks** while `c.stream` pins **1 current chunk** = **5 chunks in the active window**.
   - For 10–16 MiB chunks (`16 MiB` slab each) with `-sessionReaderCacheSizeMB=64` (`64 MiB` budget):
     - Active window = `1 pinned (16 MiB) + 4 prefetched (64 MiB) = 80 MiB > 64 MiB`!
     - Even worse, on a single slow S3 GET stream (`slow_proxy.py`), `chunk0` completes first and is placed in `b.idle` (with `pins = 1` from `c.stream`). Then `MaybeCache` launches `chunk1`, `chunk2`, `chunk3`, `chunk4`.
     - While `chunk1`, `chunk2`, and `chunk3` are **still downloading** from the volume server, they have subtracted `3 * 16 MiB = 48 MiB` from `b.used`, **but they are not in `b.idle` yet** (`b.idle` only contains completed chunks!).
     - When `chunk4`'s goroutine calls `reserveSelf(16 MiB)` (`weed/filer/reader_cache_budget.go:130`), `b.used` is `16 + 48 = 64 MiB`, so `64 + 16 = 80 > 64 MiB`. `reserveSelf` scans `b.idle` for an unpinned victim, finds **none** (because `chunk1..3` are still in flight!), falls back to `pinnedVictim` (`weed/filer/reader_cache_budget.go:141`), and **evicts `chunk0` while `c.stream` is actively reading `chunk0` in 256 KiB slices**!
     - Once `chunk1..4` finish and enter `b.idle`, `c.stream` releases `chunk0` and advances to `chunk1`, calling `MaybeCache(chunk2..5)`. `chunk5` evicts `chunk1` or `chunk2` from `b.idle`. When the slow consumer reaches `chunk2`, it has already been evicted, so `LookupOrCreate` re-downloads `chunk2` from the volume server, which evicts `chunk3`, which re-downloads `chunk3` and evicts `chunk4`—producing **continuous musical-chairs re-downloads (up to 64x in unit tests, and 3.6x–4.3x in production when mixed with faster streams)**.
   - **Fix (`7dda86092`):**
     - `ReaderCache.MaybeCache` (`weed/filer/reader_cache.go:205`) now stops scheduling lookahead chunks as soon as `pinnedBytes + windowBytes + nextCost > rc.budget.effectiveLimit()`.
     - `readerCacheBudget.reserveSelf` (`weed/filer/reader_cache_budget.go:141`) now tracks `inFlightBytes` (bytes reserved by chunks not yet added to `b.idle`) and refuses to evict a `pinnedVictim` while `inFlightBytes > 0` (waiting on `b.cond` for in-flight unpinned prefetches to land in `b.idle` so an unpinned chunk can be evicted instead, or failing the speculative prefetch if the caller is unpinned).

2. **Mechanism B — Boundary-Spanning Sequential Reads (`len(tasks) > 1`) Leaving the Newly Entered Chunk Unpinned (Fixed in `7dda86092`):**
   - `io.CopyBuffer` in `StreamContent` (`weed/filer/stream.go:257`) reads in `BufferSize = 256 KiB` slices (`32 KiB` on Filer HTTP GET).
   - Whenever a file's chunk size is not an exact multiple of `256 KiB` (for example, compressed layers, tarballs, or arbitrary Harbor blob parts), one `256 KiB` `ReadAt` call straddles the boundary between `chunk[i]` and `chunk[i+1]`, producing `len(tasks) == 2` in `ChunkReadAt.doReadAt` (`weed/filer/reader_at.go:314`).
   - Previously, `doReadAt` only passed `&c.stream` when `len(tasks) == 1`:
     - When `len(tasks) == 2`, it called `readChunkSliceAtForParallel` (`stream == nil`) for both `tasks[0]` and `tasks[1]`!
     - That left `c.stream` **still pinning the finished `chunk[i]`** (`tasks[0]`), while `chunk[i+1]` (`tasks[1]`) was acquired and immediately `Release()`d back to `pins == 0`!
     - Under budget pressure from concurrent streams or prefetches, `chunk[i+1]` (having `pins == 0`) was immediately evicted before the very next `ReadAt` call (256 KiB later), forcing `chunk[i+1]` to be downloaded a second time!
   - **Fix (`7dda86092`):** In `ChunkReadAt.doReadAt` (`weed/filer/reader_at.go:314-354`), when `len(tasks) > 1` on a sequential read (`!shouldSkipCache`), `c.releaseStream()` is called after `tasks[0]` finishes and `&lastStream` is passed for `tasks[len(tasks)-1]`, transferring `c.stream`'s pin to the newly entered chunk.

3. **Additional Read-Amplification Paths Identified in `weed/s3api` and `weed/filer`:**
   - **Clipped `ChunkView`s on unaligned HTTP `Range` GETs (`!chunkView.IsFullChunk()`):** In `ChunkReadAt.readChunkSliceAt` (`weed/filer/reader_at.go:387`), if a `ChunkView` is clipped (`ViewSize != ChunkSize`, which happens on the first/last chunk of an unaligned HTTP `Range` GET or on 64 MiB `FileChunkSection` boundaries in `weed/filer/filechunk_section.go:95`), `chunkView.IsFullChunk()` is `false`. On a disk `chunkCache` miss, it skips `readerCache` AND never calls `chunkCache.SetChunk`, issuing a separate volume-server HTTP Range GET for every `256 KiB` slice of that chunk (`32x` amplification on an 8 MiB chunk).
   - **S3 `UploadPartCopy` (`CopyObjectPartHandler`) Multi-Chunk Range Double Read:** In `CopyObjectPartHandler` (`weed/s3api/s3api_object_handlers_copy.go:1869, 1936`), `copyChunksForRange` ignores `-s3.shareCopyChunks` (unlike `CopyObjectHandler`), bypasses `ReaderCache` and `chunkCache`, and when a range spans more than one chunk (`len(Iteration) > 1`), calls `s3a.copiedRangeMd5(ctx, entry, startOffset, endOffset)` (`line 1936`) which downloads the entire source range a **second time** from the volume servers via `PrepareStreamContentWithPrefetch` just to compute the composite MD5 (**exact 2.0x read amplification**).
   - **S3 SSE GET Paths:** `streamDecryptedRangeFromChunks`, `createMultipartSSECDecryptedReaderDirect`, `createMultipartSSEKMSDecryptedReaderDirect`, and ` createMultipartSSES3DecryptedReaderDirect` (`weed/s3api/s3api_object_handlers.go`) stream via `PrepareStreamContentWithPrefetch` (`weed/filer/stream.go:180`), which fetches chunks directly via `RetriedStreamFetchChunkData` without consulting or populating `ReaderCache` or `chunkCache`.

#### Question 2: Why didn't the 2 GiB on-disk chunk cache (`-cacheCapacityMB=2048`) prevent the re-fetches?

1. **Asynchronous `writeOutChan` Gap & Head-of-Line Blocking (`weed/storage/chunk_cache/chunk_cache_tiered.go:63-112`):**
   - `SingleChunkCacher.startCaching` (`weed/filer/reader_cache.go:349`) only calls `s.parent.chunkCache.SetChunk(s.fileId, data)` after the entire chunk download completes.
   - `TieredChunkCache.SetChunk` does **not** write synchronously to disk; it pushes `{fileId, data}` into `c.writeOutChan` (buffered channel of capacity `32`), drained by a single background goroutine (`loopWriteOut`) that performs synchronous `pwrite` + BadgerDB index `Set`.
   - `TieredChunkCache.IsInCache` and `ReadChunkAt` **do not check `writeOutChan`**. When `ReaderCache` evicts a chunk under `64 MiB` budget pressure while that chunk is still sitting in `writeOutChan` waiting for disk I/O, the subsequent `ReadChunkAt` misses `TieredChunkCache` and re-fetches the chunk over HTTP from the volume server!
   - Conversely, when `writeOutChan` (cap 32) fills up during a burst, `SetChunk` blocks inside `SingleChunkCacher.startCaching` **while holding `s.Lock()` and before `close(s.done)`** (`weed/filer/reader_cache.go:349-353`), stalling every reader waiting on `<-s.done`!
2. **`GetMaxFilePartSizeInCache()` Logical Offset Cutoff (`weed/filer/reader_cache.go:230-237`, `reader_at.go:417-462`):**
   - From upstream `#6009`, `shouldCache(chunkView)` checks:
     ```go
     (uint64(chunkView.ViewOffset) + chunkView.ChunkSize) <= c.chunkCache.GetMaxFilePartSizeInCache()
     ```
   - `OnDiskCacheLayer.GetMaxFilePartSizeInCache()` returns `limit / 4` (`1/4` of each of the 4 disk sub-layers, which equals `1/4` of total `-cacheCapacityMB`).
   - With `-cacheCapacityMB=2048`, `GetMaxFilePartSizeInCache()` is **512 MiB** (and with default `-cacheCapacityMB=1000`, `250 MiB`).
   - Because `chunkView.ViewOffset` is the **logical offset in the file**, **every chunk past logical offset 512 MiB in a large container image layer or blob is completely rejected from the disk cache (`shouldCache == false`)** and relies 100% on the 64 MiB in-memory `ReaderCache`!
3. **`TieredChunkCache.memCache` 1 MiB Ceiling (`weed/s3api/s3api_server.go:375`):**
   - `NewTieredChunkCache(256, dir, diskCacheSizeInMB, 1024*1024)` sets `onDiskCacheSizeLimit0 = 1 MiB`. `TieredChunkCache` only puts chunks `<= 1 MiB` into its `memCache`; all chunks `> 1 MiB` go exclusively to `diskCaches`.

#### Question 3: How do concurrency, chunk size, and `-sessionReaderCacheSizeMB` interact?

- All concurrent S3 GET requests on an `s3_gateway` pod share a **single process-wide `readerCacheBudget`** (`weed/filer/reader_cache.go:24-39`) sized to `-sessionReaderCacheSizeMB` (default `64 MiB`).
- Because `mem.AllocationSize` rounds `(8 MiB, 16 MiB]` chunks up to `16 MiB` and `(16 MiB, 32 MiB]` chunks up to `32 MiB`:
  - With `16 MiB` slabs and a `64 MiB` process-wide budget, **at most 4 slabs fit in the entire gateway pod at once**.
  - Even a single S3 GET stream previously tried to hold `1 pinned + 4 prefetched = 5 slabs (80 MiB)`, thrashing against itself!
  - When $N \ge 4$ concurrent slow S3 GET streams read `16 MiB`-slab chunks simultaneously, `4` pinned chunks (`c.stream`) consume all `64 MiB` (`4 * 16 MiB`).
  - With commit `7dda86092`:
    - `MaybeCache` dynamically shrinks the prefetch count so `pinnedBytes + windowBytes <= effectiveLimit()` (e.g., for `16 MiB` slabs under a `64 MiB` budget, `MaybeCache` caps prefetches to `3` when 1 chunk is pinned, or `0` when 4 concurrent streams each pin a `16 MiB` chunk).
    - `reserveSelf` never evicts a `pinnedVictim` while unpinned prefetches are in flight, and when $N > 4$ concurrent readers legitimately exceed the `64 MiB` budget with their single active chunk (`c.stream`), `doReadAt` falls back cleanly without evicting another reader's active stream mid-flight.

#### Question 4: Replica Selection, Load Tracking, and Unreachable Backoff (`weed/util/http`)

- **Fixed in `5673e05da` (`weed/util/http/replica_load.go:77`):**
  - `beginReplicaRead` previously called `recordReadCost(host, time.Since(start), bytes)` even when `err != nil && bytes <= 0`. A volume server returning fast HTTP 500/503/404 or connection-reset errors in `<1 ms` had its EWMA `costNs` driven down to `replicaCostFloor` (`1 ms/MiB`), attracting *more* traffic to the failing replica unless it triggered a dial/transport error that called `recordUnreachable`.
- **Remaining Call Sites Bypassing `ReachableFirst` / `beginReplicaRead` (Documented for Follow-Up):**
  - `retriedStreamFetchChunkData` (`weed/filer/filechunk_manifest.go:427`): Calls `ReachableFirst(urlStrings)`, which sorts by `byReplicaLoad`, **but** calls `util_http.ReadUrlAsStream` without calling `beginReplicaRead` (`weed/util/http/http_global_client_util.go:271`), so streaming reads (`StreamContent` / `PrepareStreamContentWithPrefetch`) consume load scores without updating `inflight` or `costNs`!
  - `fetchChunkToBuffer` (`weed/filer/stream.go:540`), `s3a.readLocation` (`weed/s3api/s3api_object_handlers_copy_reassign.go:62`), and `readChunkRange` (`weed/s3api/s3api_object_handlers.go:2282`): Pick a single replica URL before calling `util_http`, bypassing `ReachableFirst`.

### 5.4 Slow-But-Progressing Uploads (`weed/operation`)

- **Requirement:** Uploads that are slow but progressing must not be cut off.
- **Audit Findings:**
  - Watched uploads (`option.StallTimeout > 0`, used by `weed mount`, `weed filer`, `weed s3` PUT/Copy, and `store_replicate`) use `ProgressWatch` (`weed/operation/progress_watch.go`), which resets its timer on every `Read` from the request body (`progressBody.Read`) and only cancels the attempt if **no byte moves** for `StallTimeout` (`10s`), or if no HTTP response headers arrive within `ResponseTimeout` (`20s` on mount, `30s` on gateway) **after** the entire request body has been sent (`GotFirstResponseByte` / `eofNoteReader`).
  - Unwatched uploads (`option.progress == nil`) use the `fastUploadHosts` stall watchdog (`weed/operation/upload_content.go:656-685`), which only arms when the target host recently proved a high wire rate (`bps`) and cancels only if the body has not finished draining within `uploadStallTimeout(bodySize, bps)` (`8x` slower than the host's recently proved rate, with a `10s` minimum floor).
  - **Bug Fixed in `5673e05da`:** `uploadStallTimeout(size, bps)` (`weed/operation/upload_content.go:873`) previously computed `time.Duration(size/bps) * time.Duration(uploadStallSlowFactor) * time.Second`, using integer division `size/bps` before multiplying by `uploadStallSlowFactor` (`8`). Whenever `size < bps` or `size` was not a multiple of `bps`, integer truncation shortened the stall timeout by up to `8 seconds` (clamping to `uploadStallMinWait = 10s` even when `8 * size / bps > 10s`). Fixed by computing the duration before truncating.

### 5.5 Volume `.dat` Load & Compaction Safety (`weed/storage`)

- **Requirement:** Volume load must never cut data from a `.dat`.
- **Audit Findings:**
  - Verified `Volume.load` (`weed/storage/volume_loading.go:180-226`) and `CheckVolumeDataIntegrity` (`weed/storage/volume_checking.go`): when `.idx` ends before `.dat` (`datSize > expectedDatSize`), the volume server logs a warning and **never truncates `.dat`** (tested by `TestVolumeLoadDoesNotTruncateDatFileWhenIdxIsShorter` in `weed/storage/volume_loading_test.go`).
  - Fixed the concurrency race between synced needle writes (`v.dataFileSyncLock`), `WriteNeedleBlobIfAbsent` (`weed/storage/volume_repair_missing.go:108`), and `checkCompactSource` (`weed/storage/volume_vacuum.go:196`) in commit `315ff0aff`, ensuring that no needle write or fsck missing-needle repair can be stranded on the pre-swap `.dat` file during volume compaction.
