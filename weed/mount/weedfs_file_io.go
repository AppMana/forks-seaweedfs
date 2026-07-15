package mount

import (
	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/glog"
)

/**
	 * Open a file
	 *
	 * Open flags are available in fi->flags. The following rules
	 * apply.
	 *
	 *  - Creation (O_CREAT, O_EXCL, O_NOCTTY) flags will be
	 *    filtered out / handled by the kernel.
	 *
	 *  - Access modes (O_RDONLY, O_WRONLY, O_RDWR) should be used
	 *    by the filesystem to check if the operation is
	 *    permitted.  If the ``-o default_permissions`` mount
	 *    option is given, this check is already done by the
	 *    kernel before calling open() and may thus be omitted by
	 *    the filesystem.
	 *
	 *  - When writeback caching is enabled, the kernel may send
	 *    read requests even for files opened with O_WRONLY. The
	 *    filesystem should be prepared to handle this.
	 *
	 *  - When writeback caching is disabled, the filesystem is
	 *    expected to properly handle the O_APPEND flag and ensure
	 *    that each write is appending to the end of the file.
	 *
         *  - When writeback caching is enabled, the kernel will
	 *    handle O_APPEND. However, unless all changes to the file
	 *    come through the kernel this will not work reliably. The
	 *    filesystem should thus either ignore the O_APPEND flag
	 *    (and let the kernel handle it), or return an error
	 *    (indicating that reliably O_APPEND is not available).
	 *
	 * Filesystem may store an arbitrary file handle (pointer,
	 * index, etc) in fi->fh, and use this in other all other file
	 * operations (read, write, flush, release, fsync).
	 *
	 * Filesystem may also implement stateless file I/O and not store
	 * anything in fi->fh.
	 *
	 * There are also some flags (direct_io, keep_cache) which the
	 * filesystem may set in fi, to change the way the file is opened.
	 * See fuse_file_info structure in <fuse_common.h> for more details.
	 *
	 * If this request is answered with an error code of ENOSYS
	 * and FUSE_CAP_NO_OPEN_SUPPORT is set in
	 * `fuse_conn_info.capable`, this is treated as success and
	 * future calls to open and release will also succeed without being
	 * sent to the filesystem process.
	 *
	 * Valid replies:
	 *   fuse_reply_open
	 *   fuse_reply_err
	 *
	 * @param req request handle
	 * @param ino the inode number
	 * @param fi file information
*/
func (wfs *WFS) Open(cancel <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) (status fuse.Status) {
	var fileHandle *FileHandle
	fileHandle, status = wfs.AcquireHandle(in.NodeId, in.Flags, in.Uid, in.Gid)
	if status == fuse.OK {
		out.Fh = uint64(fileHandle.fh)
		out.OpenFlags = 0

		// For read-only opens, set FOPEN_KEEP_CACHE when the file's mtime
		// has not changed since the last open.  This tells the kernel to
		// preserve its existing page cache, avoiding redundant reads.
		if in.Flags&fuse.O_ANYWRITE == 0 {
			if entry := fileHandle.GetEntry(); entry != nil && entry.Attributes != nil {
				wfs.applyKeepCacheFlag(in.NodeId, entry, out)
			}
		}
	}
	return status
}

/**
 * Release an open file
 *
 * Release is called when there are no more references to an open
 * file: all file descriptors are closed and all memory mappings
 * are unmapped.
 *
 * For every open call there will be exactly one release call (unless
 * the filesystem is force-unmounted).
 *
 * The filesystem may reply with an error, but error values are
 * not returned to close() or munmap() which triggered the
 * release.
 *
 * fi->fh will contain the value set by the open method, or will
 * be undefined if the open method didn't set any value.
 * fi->flags will contain the same flags as for open.
 *
 * Valid replies:
 *   fuse_reply_err
 *
 * @param req request handle
 * @param ino the inode number
 * @param fi file information
 */
const openMtimeCacheMaxSize = 8192

// invalidatedMtime is a sentinel stored in openMtimeCache by
// invalidateOpenMtimeCache to mark an inode as "known dirty" -- distinct
// from an inode that was never recorded at all. No real FuseAttributes ever
// produces this pair (MtimeNs is a wall-clock nanosecond-of-second value,
// always >= 0), so it can never equal a genuine currentMtime.
var invalidatedMtime = [2]int64{-1, -1}

// applyKeepCacheFlag compares the entry's mtime (seconds + nanoseconds)
// against the last-seen value recorded by this WFS process and sets
// FOPEN_KEEP_CACHE unless we have positive evidence the file changed since
// we last saw it.
//
// Two cases set the flag: mtime unchanged since a previous open we recorded
// (the original, narrow case), and -- the fix here -- an inode this process
// has never opened before (!loaded). The latter matters because a one-shot
// workload (open once, mmap, read, close -- e.g. loading an ML checkpoint)
// never gets a "previous open" to compare against under the old logic, so
// FOPEN_KEEP_CACHE was never set for it, and FUSE's default (no keep_cache)
// means the kernel discards page-cache state for that inode across separate
// opens.
//
// A *changed* mtime, or an inode explicitly marked dirty by
// invalidateOpenMtimeCache (loaded && prev != currentMtime, which includes
// prev == invalidatedMtime), withholds the flag -- that's the case the
// mechanism actually exists to guard: a write (local, via
// invalidateOpenMtimeCache in weedfs_file_write.go / weedfs_file_mkrm.go /
// weedfs_attr.go, or external, reflected in a fresh entry.Attributes.Mtime
// fetched at this open) since we last recorded this inode's mtime. The
// sentinel matters because a plain delete-on-invalidate would make the next
// open indistinguishable from "never opened," which would incorrectly set
// keep_cache right after a write we just flagged as unsafe to cache.
//
// Accepted tradeoff: if this weed mount process restarts, openMtimeCache
// resets to empty, so the immediately-following open of an inode the kernel
// still has stale cached pages for (from before the restart) will look like
// "never seen" and get keep_cache set optimistically. This is a narrow
// window (requires an external modification during the restart itself) and
// was already an accepted risk class for any keep_cache-based scheme; it is
// not made worse by treating first-open as safe rather than unsafe.
func (wfs *WFS) applyKeepCacheFlag(inode uint64, entry *LockedEntry, out *fuse.OpenOut) {
	currentMtime := [2]int64{entry.Attributes.Mtime, int64(entry.Attributes.MtimeNs)}
	wfs.openMtimeMu.Lock()
	if wfs.openMtimeCache == nil {
		wfs.openMtimeCache = make(map[uint64][2]int64, 8192)
	}
	prev, loaded := wfs.openMtimeCache[inode]
	if !loaded || prev == currentMtime {
		out.OpenFlags |= fuse.FOPEN_KEEP_CACHE
	}
	if len(wfs.openMtimeCache) >= openMtimeCacheMaxSize {
		for k := range wfs.openMtimeCache {
			delete(wfs.openMtimeCache, k)
			break
		}
	}
	wfs.openMtimeCache[inode] = currentMtime
	wfs.openMtimeMu.Unlock()
}

// invalidateOpenMtimeCache marks an inode's cached mtime as dirty so the
// next Open does not set FOPEN_KEEP_CACHE with stale kernel page cache data.
// It stores a sentinel rather than deleting the entry -- see invalidatedMtime.
func (wfs *WFS) invalidateOpenMtimeCache(inode uint64) {
	wfs.openMtimeMu.Lock()
	if wfs.openMtimeCache == nil {
		wfs.openMtimeCache = make(map[uint64][2]int64, 8192)
	}
	wfs.openMtimeCache[inode] = invalidatedMtime
	wfs.openMtimeMu.Unlock()
}

func (wfs *WFS) Release(cancel <-chan struct{}, in *fuse.ReleaseIn) {
	// Flush is usually sent before Release, but the FUSE protocol does not
	// guarantee it. Route every Release through doFlush so a dirty handle
	// (e.g. a deferred create with no intervening Flush) is not dropped.
	// doFlush itself inspects dirtyMetadata / asyncFlushPending and fast-paths
	// the clean case, so the duplicate call after a normal Flush is cheap.
	if fh := wfs.GetHandle(FileHandleId(in.Fh)); fh != nil {
		allowAsync := in.ReleaseFlags&fuse.FUSE_RELEASE_FLOCK_UNLOCK == 0
		if status := wfs.doFlush(fh, in.Uid, in.Gid, allowAsync); status != fuse.OK {
			glog.Warningf("release fh %d inode %d: fallback flush failed: %v", in.Fh, in.NodeId, status)
		}
	}
	if in.ReleaseFlags&fuse.FUSE_RELEASE_FLOCK_UNLOCK != 0 {
		wfs.posixLocks.ReleaseFlockOwner(in.NodeId, in.LockOwner)
	}
	wfs.ReleaseHandle(FileHandleId(in.Fh))
}
