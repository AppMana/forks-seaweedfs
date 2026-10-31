package mount

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"time"

	cgofuse "github.com/winfsp/cgofuse/fuse"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// invalidFh is the cgofuse "no file handle" sentinel (~uint64(0)).
const invalidFh = ^uint64(0)

const (
	// winfspCacheTTL is how long the WinFsp adapter keeps a file's lookup
	// reference after its last use, and how long cached attributes answer.
	winfspCacheTTL = time.Second

	// winfspMaxCachedPaths bounds the references the path cache holds.
	winfspMaxCachedPaths = 64 << 10
)

// winfspFS adapts the inode-based WFS raw handlers to cgofuse's
// path-based FileSystemInterface, served by WinFsp on Windows. All
// filesystem logic (chunk reads, dirty-page writes, rename streaming,
// meta cache) stays in WFS; this layer only translates paths to inodes
// and cgofuse structs to go-fuse structs.
//
// Every inode the adapter uses is covered by a lookup reference, the way the
// kernel holds one for each dentry and open file on the unix mounts: an
// operation holds one for its duration, an open handle until Release, and
// the path cache one per cached path. WinFsp sends no FORGET, so each is
// returned with WFS.Forget, and the inode table only holds what is in use.
type winfspFS struct {
	cgofuse.FileSystemBase
	wfs              *WFS
	readAhead        *readAheadCache
	paths            *winfspPathCache[cgofuse.Stat_t]
	caseSensitive    bool
	identityError    error
	basicPermissions bool
	// WinFsp has no read-only option for FUSE file systems, so the adapter
	// refuses mutations itself.
	readOnly bool

	// The references open handles hold, by handle. WinFsp hands back only
	// the handle, so these are what Release returns.
	handleMu   sync.Mutex
	fileHandle map[uint64]*winfspHandleRef
	dirHandle  map[uint64]*winfspHandleRef
	sweepStop  chan struct{}
	sweepDone  chan struct{}
}

// winfspHandleRef counts the opens a handle carries a reference for.
type winfspHandleRef struct {
	inode uint64
	count int
}

func newWinfspFS(wfs *WFS, caseSensitive bool) *winfspFS {
	a := &winfspFS{
		wfs:           wfs,
		readAhead:     newReadAheadCache(),
		caseSensitive: caseSensitive,
		fileHandle:    make(map[uint64]*winfspHandleRef),
		dirHandle:     make(map[uint64]*winfspHandleRef),
	}
	a.paths = newWinfspPathCache[cgofuse.Stat_t](winfspCacheTTL, wfs.dirIdleEvict, winfspMaxCachedPaths,
		func(inode uint64) bool { return wfs.inodeToPath.IncrementNlookup(inode) },
		a.release, a.inodeBusy)
	return a
}

// expireReferences returns the lookup references whose retention ended
// before now.
func (a *winfspFS) expireReferences(now time.Time) {
	a.paths.sweep(now)
}

// startReferenceSweeper runs for the mounted filesystem's Init/Destroy lifetime.
func (a *winfspFS) startReferenceSweeper() {
	if a.sweepStop != nil {
		return
	}
	a.sweepStop, a.sweepDone = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(a.sweepDone)
		ticker := time.NewTicker(winfspCacheTTL)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				a.expireReferences(now)
			case <-a.sweepStop:
				return
			}
		}
	}()
}

// entryChanged applies a metadata event to the adapter's own caches. A name
// that went away or moved takes its cached reference with it, and a vacated
// directory its subtree's. A name that appeared replaces whatever was cached
// under it.
func (a *winfspFS) entryChanged(event meta_cache.EntryInvalidation) {
	// Every event outdates the attributes of its path, of the inode's other
	// names (a hard link's nlink and contents), and of the parent directory.
	if ino, found := a.wfs.inodeToPath.GetInode(event.Path); found {
		a.invalidateInode(ino)
		if event.Deleted && event.Entry == nil && event.RenamedTo == "" && event.DeletedInode != 0 && event.DeletedInode == ino {
			a.wfs.inodeToPath.RemovePathForInode(event.Path, ino)
		}
	}
	if event.RenamedTo != "" {
		if ino, found := a.wfs.inodeToPath.GetInode(event.RenamedTo); found {
			a.invalidateInode(ino)
		}
	}
	a.forgetStatWithParent(event.Path)
	if event.RenamedTo != "" {
		a.forgetStatWithParent(event.RenamedTo)
	}
	if event.Entry != nil && event.PreviousEntry != nil {
		return // an update in place keeps the name
	}
	for _, ino := range a.paths.purge(event.Path, event.Entry == nil && event.WasDirectory) {
		a.invalidateInode(ino)
	}
	if event.RenamedTo != "" {
		for _, ino := range a.paths.purge(event.RenamedTo, false) {
			a.invalidateInode(ino)
		}
	}
}

// forgetStatWithParent drops the attributes cached for key and for its
// directory, whose mtime a change of the name moves.
func (a *winfspFS) forgetStatWithParent(key util.FullPath) {
	a.paths.forgetStat(key)
	if dir, _ := key.DirAndName(); dir != string(key) {
		a.paths.forgetStat(util.FullPath(dir))
	}
}

// forgetInodeStat drops the attributes cached under every name of ino.
func (a *winfspFS) forgetInodeStat(ino uint64) {
	for _, path := range a.wfs.inodeToPath.GetAllPaths(ino) {
		a.paths.forgetStat(path)
	}
}

// invalidateInode drops cached attributes and read-ahead data for ino.
func (a *winfspFS) invalidateInode(ino uint64) {
	if ino <= 1 {
		return
	}
	a.forgetInodeStat(ino)
	a.readAhead.Invalidate(ino)
}

// fullPath converts a cgofuse path ('/'-separated, relative to the
// mount point) to the filer-space path that inodeToPath tracks.
func (a *winfspFS) fullPath(path string) util.FullPath {
	root := a.wfs.option.FilerMountRootPath
	if path == "/" || path == "" {
		return util.FullPath(root)
	}
	if root == "/" {
		return util.FullPath(path)
	}
	return util.FullPath(root + path)
}

// resolve walks a cgofuse path to its inode and takes a lookup reference on
// it, which the caller returns with release. The root holds none.
func (a *winfspFS) resolve(path string) (uint64, fuse.Status) {
	ino, _, _, status := a.walk(strings.Trim(path, "/"))
	return ino, status
}

// walk resolves rel, a path relative to the mount root without leading or
// trailing separators, to a referenced inode and its canonical spelling. A
// tracked path is taken as is; otherwise the parent is walked and the leaf
// looked up, and the cache keeps the result for the next walk. It also
// reports whether the inode is a directory: a path through a file is ENOTDIR.
func (a *winfspFS) walk(rel string) (uint64, string, bool, fuse.Status) {
	if rel == "" {
		return 1, "", true, fuse.OK
	}
	full := a.fullPath("/" + rel)
	if ino, found := a.wfs.inodeToPath.AcquirePath(full); found {
		a.paths.touch(full, time.Now())
		dir, known := a.paths.isDir(full)
		if !known {
			dir = a.inodeIsDir(ino)
		}
		return ino, rel, dir, fuse.OK
	}
	parentRel, name := "", rel
	if idx := strings.LastIndex(rel, "/"); idx >= 0 {
		parentRel, name = rel[:idx], rel[idx+1:]
	}
	parent, parentCanonical, parentDir, status := a.walk(parentRel)
	if status != fuse.OK {
		return 0, "", false, status
	}
	defer a.release(parent)
	if !parentDir {
		return 0, "", false, fuse.ENOTDIR
	}
	child, canonical, dir, status := a.lookupChild(parent, name)
	if status != fuse.OK {
		glog.V(1).Infof("winfsp resolve failed: path=%q parent_inode=%d component=%q status=%v", rel, parent, name, status)
		return 0, "", false, status
	}
	if parentCanonical != "" {
		canonical = parentCanonical + "/" + canonical
	}
	a.paths.hold(a.fullPath("/"+canonical), child, dir, time.Now())
	return child, canonical, dir, fuse.OK
}

// inodeIsDir reads whether a tracked inode the path cache does not hold is a
// directory. An inode whose attributes cannot be read is taken as one, so the
// operation itself reports the failure.
func (a *winfspFS) inodeIsDir(ino uint64) bool {
	in := fuse.GetAttrIn{InHeader: a.header(ino)}
	var out fuse.AttrOut
	if a.wfs.GetAttr(nil, &in, &out) != fuse.OK {
		return true
	}
	return out.Attr.Mode&cgofuse.S_IFMT == cgofuse.S_IFDIR
}

// release returns one lookup reference. The root never takes one.
func (a *winfspFS) release(ino uint64) {
	if ino > 1 {
		a.wfs.Forget(ino, 1)
	}
}

// inodeBusy reports an inode whose last reference must not go back yet: an
// open or draining handle, or a pending async flush, still needs its path.
func (a *winfspFS) inodeBusy(ino uint64) bool {
	if a.wfs.IsFileOpen(ino) {
		return true
	}
	a.wfs.pendingAsyncFlushMu.Lock()
	_, pending := a.wfs.pendingAsyncFlush[ino]
	a.wfs.pendingAsyncFlushMu.Unlock()
	return pending
}

// park hands a reference the caller owns to the path cache under the inode's
// current path, so the next operation on it resolves at once. An inode with
// no path left goes back, held while busy.
func (a *winfspFS) park(ino uint64, dir bool) {
	if ino <= 1 {
		return
	}
	if path, status := a.wfs.inodeToPath.GetPath(ino); status == fuse.OK {
		a.paths.adopt(path, ino, dir, time.Now())
		return
	}
	a.paths.release(ino)
}

// retainHandle records the reference an open handle now carries.
func (a *winfspFS) retainHandle(table map[uint64]*winfspHandleRef, fh, ino uint64) {
	if fh == invalidFh {
		a.release(ino)
		return
	}
	var stranded uint64
	a.handleMu.Lock()
	switch existing, found := table[fh]; {
	case found && existing.inode == ino:
		existing.count++
	case found:
		// A handle number reissued for another inode; its old reference
		// would otherwise never be returned.
		stranded = existing.inode
		table[fh] = &winfspHandleRef{inode: ino, count: 1}
	default:
		table[fh] = &winfspHandleRef{inode: ino, count: 1}
	}
	a.handleMu.Unlock()
	if stranded != 0 {
		a.paths.release(stranded)
	}
}

// releaseHandle drops one open of fh and reports the inode once the last one
// is gone, so a repeated release cannot return the reference twice.
func (a *winfspFS) releaseHandle(table map[uint64]*winfspHandleRef, fh uint64) uint64 {
	a.handleMu.Lock()
	defer a.handleMu.Unlock()
	existing, found := table[fh]
	if !found {
		return 0
	}
	existing.count--
	if existing.count > 0 {
		return 0
	}
	delete(table, fh)
	return existing.inode
}

// handleInode reports the inode an open handle holds, or 0.
func (a *winfspFS) handleInode(table map[uint64]*winfspHandleRef, fh uint64) uint64 {
	if fh == invalidFh {
		return 0
	}
	a.handleMu.Lock()
	defer a.handleMu.Unlock()
	if existing, found := table[fh]; found {
		return existing.inode
	}
	return 0
}

// Getpath supplies WinFsp's normalized name for a case-insensitive mount.
// Without it WinFsp falls back to an uppercased name in file information and
// directory-change notifications, even though the filer preserves spelling.
// Resolve the requested link rather than an inode's primary path: hard links
// must not silently acquire another link's name here.
func (a *winfspFS) Getpath(path string, _ uint64) (int, string) {
	if path == "/" || path == "" {
		return 0, "/"
	}
	if _, found := a.wfs.inodeToPath.GetInode(a.fullPath(path)); found {
		return 0, path
	}
	ino, canonical, _, status := a.walk(strings.Trim(path, "/"))
	if status != fuse.OK {
		return toWinErrno(status), ""
	}
	a.release(ino)
	return 0, "/" + canonical
}

// lookupChild resolves a child exactly first. Case-folded lookups are only
// used for mismatched Windows paths, keeping the normal lookup fast. On
// success the caller owns the lookup reference on the child.
func (a *winfspFS) lookupChild(parent uint64, requested string) (uint64, string, bool, fuse.Status) {
	var out fuse.EntryOut
	status := a.wfs.Lookup(nil, &fuse.InHeader{NodeId: parent}, requested, &out)
	if status == fuse.OK {
		return out.NodeId, requested, out.Attr.Mode&cgofuse.S_IFMT == cgofuse.S_IFDIR, fuse.OK
	}
	if a.caseSensitive || status != fuse.ENOENT {
		return 0, "", false, status
	}

	parentPath, status := a.wfs.inodeToPath.GetPath(parent)
	if status != fuse.OK {
		return 0, "", false, status
	}
	actual, err := a.wfs.findCaseFoldedChild(context.Background(), parentPath, requested)
	if err != nil {
		glog.Errorf("winfsp case-fold listing %s: %v", parentPath, err)
		return 0, "", false, fuse.EIO
	}
	if actual == "" {
		glog.V(1).Infof("winfsp case-fold miss: parent=%q requested=%q", parentPath, requested)
		return 0, "", false, fuse.ENOENT
	}

	out = fuse.EntryOut{}
	if status = a.wfs.Lookup(nil, &fuse.InHeader{NodeId: parent}, actual, &out); status != fuse.OK {
		glog.V(1).Infof("winfsp canonical lookup failed: parent=%q requested=%q canonical=%q status=%v", parentPath, requested, actual, status)
		return 0, "", false, status
	}
	return out.NodeId, actual, out.Attr.Mode&cgofuse.S_IFMT == cgofuse.S_IFDIR, fuse.OK
}

// canonicalChildName reports the existing child's spelling, or ENOENT.
func (a *winfspFS) canonicalChildName(parent uint64, requested string) (uint64, string, fuse.Status) {
	child, canonical, _, status := a.lookupChild(parent, requested)
	if status != fuse.OK {
		return 0, "", status
	}
	a.release(child)
	return child, canonical, fuse.OK
}

// resolveParent splits a cgofuse path into the parent inode, referenced for
// the caller to release, and the leaf name.
func (a *winfspFS) resolveParent(path string) (uint64, string, fuse.Status) {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return 0, "", fuse.EINVAL
	}
	dir, name := "/", trimmed
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		dir, name = "/"+trimmed[:idx], trimmed[idx+1:]
	}
	parent, _, isDir, st := a.walk(strings.Trim(dir, "/"))
	if st != fuse.OK {
		return 0, "", st
	}
	if !isDir {
		a.release(parent)
		return 0, "", fuse.ENOTDIR
	}
	return parent, name, fuse.OK
}

// resolveExisting resolves path's parent and target child, holding a lookup
// reference on both for the caller to release. Keeping the child referenced
// across the mutation ensures WFS sees it in inodeToPath (for MovePath and
// hard-link sibling updates) even when the path cache had not retained it.
func (a *winfspFS) resolveExisting(path string) (uint64, uint64, string, bool, fuse.Status) {
	parent, requested, status := a.resolveParent(path)
	if status != fuse.OK {
		return 0, 0, "", false, status
	}
	child, actual, dir, status := a.lookupChild(parent, requested)
	if status != fuse.OK {
		a.release(parent)
		return 0, 0, requested, false, status
	}
	return parent, child, actual, dir, fuse.OK
}

func (a *winfspFS) resolveMutationName(parent uint64, requested string) (string, bool, fuse.Status) {
	if a.caseSensitive {
		return requested, false, fuse.OK
	}
	_, canonical, status := a.canonicalChildName(parent, requested)
	if status == fuse.ENOENT {
		return requested, false, fuse.OK
	}
	if status != fuse.OK {
		return requested, false, status
	}
	name, exists := winFspMutationName(requested, canonical, true, false)
	return name, exists, fuse.OK
}

func (a *winfspFS) header(ino uint64) fuse.InHeader {
	return fuse.InHeader{
		NodeId: ino,
		Caller: fuse.Caller{
			Owner: fuse.Owner{Uid: a.wfs.option.MountUid, Gid: a.wfs.option.MountGid},
		},
	}
}

// inodeFromFh maps an open cgofuse file handle back to its inode.
func (a *winfspFS) inodeFromFh(fh uint64) (uint64, bool) {
	if fh == invalidFh {
		return 0, false
	}
	if h := a.wfs.GetHandle(FileHandleId(fh)); h != nil {
		return h.inode, true
	}
	return 0, false
}

func attrToStat(attr *fuse.Attr, st *cgofuse.Stat_t) {
	// Stat_t buffers may be reused. Never inherit another entry's Windows
	// attributes, and report the normal archive flag for a regular file.
	st.Flags = 0
	if attr.Mode&cgofuse.S_IFMT == cgofuse.S_IFREG {
		st.Flags = cgofuse.UF_ARCHIVE
	}
	st.Ino = attr.Ino
	st.Mode = attr.Mode
	st.Nlink = attr.Nlink
	st.Uid = attr.Uid
	st.Gid = attr.Gid
	st.Rdev = uint64(attr.Rdev)
	st.Size = int64(attr.Size)
	st.Atim = cgofuse.Timespec{Sec: int64(attr.Atime), Nsec: int64(attr.Atimensec)}
	st.Mtim = cgofuse.Timespec{Sec: int64(attr.Mtime), Nsec: int64(attr.Mtimensec)}
	st.Ctim = cgofuse.Timespec{Sec: int64(attr.Ctime), Nsec: int64(attr.Ctimensec)}
	// The Linux FUSE attr has no creation time; WinFsp wants one.
	st.Birthtim = st.Ctim
	st.Blksize = int64(attr.Blksize)
	if st.Blksize == 0 {
		st.Blksize = blockSize
	}
	st.Blocks = int64(attr.Blocks)
}

func (a *winfspFS) Destroy() {
	if a.sweepStop != nil {
		close(a.sweepStop)
		<-a.sweepDone
		a.sweepStop = nil
	}
	writeWinfspStatsTrace()
}

func (a *winfspFS) Statfs(path string, stat *cgofuse.Statfs_t) int {
	defer track(opStatfs)()
	var out fuse.StatfsOut
	if st := a.wfs.StatFs(nil, &fuse.InHeader{NodeId: 1}, &out); st != fuse.OK {
		return toWinErrno(st)
	}
	stat.Bsize = uint64(out.Bsize)
	stat.Frsize = uint64(out.Frsize)
	if stat.Frsize == 0 {
		stat.Frsize = stat.Bsize
	}
	stat.Blocks = out.Blocks
	stat.Bfree = out.Bfree
	stat.Bavail = out.Bavail
	stat.Files = out.Files
	stat.Ffree = out.Ffree
	stat.Namemax = uint64(out.NameLen)
	return 0
}

// Getattr answers from the path cache's attributes the way the kernel's
// attribute cache answers on the unix mounts. A file with an open handle, or
// a flush still pending, is excluded: its live size is on the handle.
func (a *winfspFS) Getattr(path string, st *cgofuse.Stat_t, fh uint64) int {
	defer track(opGetattr)()
	if a.identityError != nil {
		return -cgofuse.EIO
	}
	if ino, ok := a.inodeFromFh(fh); ok {
		return a.getattrInode(path, ino, fh, st)
	}
	now := time.Now()
	full := a.fullPath(path)
	if ino, cached, ok := a.paths.stat(full, now); ok && !a.inodeBusy(ino) {
		if current, found := a.wfs.inodeToPath.GetInode(full); found && current == ino {
			*st = cached
			return 0
		}
	}
	gen := a.paths.generation()
	ino, canonical, _, status := a.walk(strings.Trim(path, "/"))
	if status != fuse.OK {
		return toWinErrno(status)
	}
	defer a.release(ino)
	if code := a.getattrInode(path, ino, fh, st); code != 0 {
		return code
	}
	if ino > 1 && !a.inodeBusy(ino) {
		a.paths.setStat(a.fullPath("/"+canonical), ino, st.Mode&cgofuse.S_IFMT == cgofuse.S_IFDIR, *st, gen, now)
	}
	return 0
}

func (a *winfspFS) getattrInode(path string, ino, fh uint64, st *cgofuse.Stat_t) int {
	in := fuse.GetAttrIn{InHeader: a.header(ino)}
	var out fuse.AttrOut
	if status := a.wfs.GetAttr(nil, &in, &out); status != fuse.OK {
		glog.V(1).Infof("winfsp getattr failed: path=%q inode=%d fh=%d status=%v", path, ino, fh, status)
		return toWinErrno(status)
	}
	attrToStat(&out.Attr, st)
	return a.getattrWindowsFlags(ino, st)
}

// forgetChildStat drops the attributes cached for name under parent and for
// parent itself, after a mutation of the name.
func (a *winfspFS) forgetChildStat(parent uint64, name string) {
	if key, keyed := a.childKey(parent, name); keyed {
		a.forgetStatWithParent(key)
	}
}

// childKey is the cached path of name under a referenced parent.
func (a *winfspFS) childKey(parent uint64, name string) (util.FullPath, bool) {
	parentPath, status := a.wfs.inodeToPath.GetPath(parent)
	if status != fuse.OK {
		return "", false
	}
	return parentPath.Child(name), true
}

func (a *winfspFS) Mkdir(path string, mode uint32) int {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	defer track(opMkdir)()
	parent, name, st := a.resolveParent(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	defer a.release(parent)
	name, exists, st := a.resolveMutationName(parent, name)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	if exists {
		return -cgofuse.EEXIST
	}
	var out fuse.EntryOut
	in := fuse.MkdirIn{InHeader: a.creationHeader(parent), Mode: mode & 07777}
	status := a.wfs.Mkdir(nil, &in, name, &out)
	a.forgetChildStat(parent, name)
	if status == fuse.OK {
		// The new directory is about to be filled; keep its reference.
		a.park(out.NodeId, true)
	}
	return toWinErrno(status)
}

// WinFsp converts NFS special-file reparse points through Mknod and Rename.
// Store the type and device number in the filer; no host device is created.
func (a *winfspFS) Mknod(path string, mode uint32, device uint64) int {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	if device > uint64(^uint32(0)) {
		return -cgofuse.EINVAL
	}
	if a.wfs.IsOverQuotaWithUncommitted() {
		return -cgofuse.ENOSPC
	}
	parent, name, status := a.resolveParent(path)
	if status != fuse.OK {
		return toWinErrno(status)
	}
	defer a.release(parent)
	name, exists, status := a.resolveMutationName(parent, name)
	if status != fuse.OK {
		return toWinErrno(status)
	}
	if exists {
		return -cgofuse.EEXIST
	}
	in := fuse.MknodIn{InHeader: a.creationHeader(parent), Mode: mode, Rdev: uint32(device)}
	var out fuse.EntryOut
	status = a.wfs.Mknod(nil, &in, name, &out)
	a.forgetChildStat(parent, name)
	if status == fuse.OK {
		a.park(out.NodeId, false)
	}
	return toWinErrno(status)
}

func (a *winfspFS) Rmdir(path string) int {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	defer track(opRmdir)()
	parent, child, name, _, st := a.resolveExisting(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	defer a.release(parent)
	defer a.release(child)
	key, keyed := a.childKey(parent, name)
	hdr := a.header(parent)
	status := a.wfs.Rmdir(nil, &hdr, name)
	a.forgetChildStat(parent, name)
	if status == fuse.OK && keyed {
		a.paths.purge(key, true)
	}
	return toWinErrno(status)
}

func (a *winfspFS) Unlink(path string) int {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	defer track(opUnlink)()
	parent, child, name, _, st := a.resolveExisting(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	defer a.release(parent)
	defer a.release(child)
	key, keyed := a.childKey(parent, name)
	hdr := a.header(parent)
	status := a.wfs.Unlink(nil, &hdr, name)
	// The inode's other names lose a link, and any cached read-ahead is gone.
	a.invalidateInode(child)
	a.forgetChildStat(parent, name)
	if status == fuse.OK && keyed {
		a.paths.purge(key, false)
	}
	return toWinErrno(status)
}

func (a *winfspFS) Rename(oldpath string, newpath string) (result int) {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	defer track(opRename)()
	stage := "resolve source"
	defer func() {
		if result != 0 {
			glog.V(1).Infof("winfsp rename failed at %s: %q => %q: errno %d", stage, oldpath, newpath, result)
		}
	}()
	oldParent, oldIno, oldName, oldDir, st := a.resolveExisting(oldpath)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	defer a.release(oldParent)
	stage = "resolve destination parent"
	newParent, newName, st := a.resolveParent(newpath)
	if st != fuse.OK {
		a.release(oldIno)
		return toWinErrno(st)
	}
	defer a.release(newParent)
	targetIno, canonicalNewName, _, newStatus := a.lookupChild(newParent, newName)
	switch newStatus {
	case fuse.OK:
		defer a.release(targetIno)
		// Preserve the requested spelling for a case-only rename of the
		// same object. Otherwise replace the existing canonical target.
		if !a.caseSensitive && targetIno != oldIno {
			newName = canonicalNewName
		}
	case fuse.ENOENT:
	default:
		a.release(oldIno)
		stage = "lookup destination"
		return toWinErrno(newStatus)
	}
	oldKey, oldKeyed := a.childKey(oldParent, oldName)
	newKey, newKeyed := a.childKey(newParent, newName)
	in := fuse.RenameIn{
		InHeader: a.header(oldParent),
		Newdir:   newParent,
		// Flags 0 == overwrite allowed, matching Windows ReplaceIfExists.
	}
	stage = "core rename"
	status := a.wfs.Rename(nil, &in, oldName, newName)
	a.invalidateInode(oldIno)
	if targetIno != 0 && targetIno != oldIno {
		a.invalidateInode(targetIno)
	}
	a.forgetChildStat(oldParent, oldName)
	a.forgetChildStat(newParent, newName)
	if status == fuse.OK {
		// A directory carries its subtree's names with it, and the rename may
		// have replaced whatever held the destination.
		if oldKeyed {
			a.paths.purge(oldKey, true)
		}
		if newKeyed {
			a.paths.purge(newKey, true)
			a.paths.adopt(newKey, oldIno, oldDir, time.Now())
			return toWinErrno(status)
		}
	}
	a.release(oldIno)
	return toWinErrno(status)
}

func (a *winfspFS) Symlink(target string, newpath string) int {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	parent, name, st := a.resolveParent(newpath)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	defer a.release(parent)
	name, exists, st := a.resolveMutationName(parent, name)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	if exists {
		return -cgofuse.EEXIST
	}
	var out fuse.EntryOut
	hdr := a.creationHeader(parent)
	status := a.wfs.Symlink(nil, &hdr, a.wfs.mountRootTarget(target), name, &out)
	a.forgetChildStat(parent, name)
	if status == fuse.OK {
		a.park(out.NodeId, false)
	}
	return toWinErrno(status)
}

func (a *winfspFS) Readlink(path string) (int, string) {
	ino, st := a.resolve(path)
	if st != fuse.OK {
		return toWinErrno(st), ""
	}
	defer a.release(ino)
	hdr := a.header(ino)
	out, status := a.wfs.Readlink(nil, &hdr)
	if status != fuse.OK {
		return toWinErrno(status), ""
	}
	return 0, string(out)
}

func (a *winfspFS) Link(oldpath string, newpath string) int {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	oldIno, st := a.resolve(oldpath)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	defer a.release(oldIno)
	parent, name, st := a.resolveParent(newpath)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	defer a.release(parent)
	name, exists, st := a.resolveMutationName(parent, name)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	if exists {
		return -cgofuse.EEXIST
	}
	var out fuse.EntryOut
	in := fuse.LinkIn{InHeader: a.header(parent), Oldnodeid: oldIno}
	status := a.wfs.Link(nil, &in, name, &out)
	a.forgetInodeStat(oldIno)
	a.forgetChildStat(parent, name)
	if status == fuse.OK {
		if key, keyed := a.childKey(parent, name); keyed {
			a.paths.adopt(key, out.NodeId, false, time.Now())
		} else {
			a.park(out.NodeId, false)
		}
	}
	return toWinErrno(status)
}

// setAttr applies a path-addressed attribute change.
func (a *winfspFS) setAttr(path string, common fuse.SetAttrInCommon) int {
	ino, st := a.resolve(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	defer a.release(ino)
	common.InHeader = a.header(ino)
	var out fuse.AttrOut
	status := a.wfs.SetAttr(nil, &fuse.SetAttrIn{SetAttrInCommon: common}, &out)
	a.forgetInodeStat(ino)
	return toWinErrno(status)
}

func (a *winfspFS) Chmod(path string, mode uint32) int {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	return a.setAttr(path, fuse.SetAttrInCommon{Valid: fuse.FATTR_MODE, Mode: mode})
}

func (a *winfspFS) Chown(path string, uid uint32, gid uint32) int {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	var valid uint32
	if int32(uid) != -1 {
		valid |= fuse.FATTR_UID
	}
	if int32(gid) != -1 {
		valid |= fuse.FATTR_GID
	}
	if valid == 0 {
		// Resolve anyway: a missing path still reports ENOENT.
		ino, st := a.resolve(path)
		if st != fuse.OK {
			return toWinErrno(st)
		}
		a.release(ino)
		return 0
	}
	return a.setAttr(path, fuse.SetAttrInCommon{Valid: valid, Owner: fuse.Owner{Uid: uid, Gid: gid}})
}

func (a *winfspFS) Utimens(path string, tmsp []cgofuse.Timespec) int {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	var common fuse.SetAttrInCommon
	if len(tmsp) >= 2 {
		if winfspTimeApplies(tmsp[0].Sec, tmsp[0].Nsec) {
			common.Valid |= fuse.FATTR_ATIME
			common.Atime, common.Atimensec = uint64(tmsp[0].Sec), uint32(tmsp[0].Nsec)
		}
		if winfspTimeApplies(tmsp[1].Sec, tmsp[1].Nsec) {
			common.Valid |= fuse.FATTR_MTIME
			common.Mtime, common.Mtimensec = uint64(tmsp[1].Sec), uint32(tmsp[1].Nsec)
		}
		if common.Valid == 0 {
			ino, st := a.resolve(path)
			if st != fuse.OK {
				return toWinErrno(st)
			}
			a.release(ino)
			return 0
		}
	} else {
		now := cgofuse.Now()
		common.Valid = fuse.FATTR_ATIME | fuse.FATTR_MTIME | fuse.FATTR_ATIME_NOW | fuse.FATTR_MTIME_NOW
		common.Atime, common.Atimensec = uint64(now.Sec), uint32(now.Nsec)
		common.Mtime, common.Mtimensec = uint64(now.Sec), uint32(now.Nsec)
	}
	return a.setAttr(path, common)
}

func (a *winfspFS) Access(path string, mask uint32) int {
	// WinFsp enforces access using the Windows caller's token before
	// dispatch. WFS's synthetic Unix identity cannot model that token or
	// its backup/restore privileges. Independent WFS protections still apply.
	return 0
}

func (a *winfspFS) Create(path string, flags int, mode uint32) (int, uint64) {
	if a.readOnly {
		return -cgofuse.EROFS, invalidFh
	}
	defer track(opCreate)()
	parent, name, st := a.resolveParent(path)
	if st != fuse.OK {
		return toWinErrno(st), invalidFh
	}
	defer a.release(parent)
	name, exists, st := a.resolveMutationName(parent, name)
	if st != fuse.OK {
		return toWinErrno(st), invalidFh
	}
	if exists && flags&cgofuse.O_EXCL != 0 {
		return -cgofuse.EEXIST, invalidFh
	}
	in := fuse.CreateIn{
		InHeader: a.creationHeader(parent),
		Flags:    toGoOpenFlags(flags),
		Mode:     mode & 07777,
	}
	var out fuse.CreateOut
	status := a.wfs.Create(nil, &in, name, &out)
	a.forgetChildStat(parent, name)
	if status != fuse.OK {
		return toWinErrno(status), invalidFh
	}
	a.invalidateInode(out.NodeId)
	// Create grants a reference on the inode; the handle carries it.
	a.retainHandle(a.fileHandle, out.Fh, out.NodeId)
	return 0, out.Fh
}

func (a *winfspFS) Open(path string, flags int) (int, uint64) {
	defer track(opOpen)()
	ino, st := a.resolve(path)
	if st != fuse.OK {
		return toWinErrno(st), invalidFh
	}
	in := fuse.OpenIn{InHeader: a.header(ino), Flags: toGoOpenFlags(flags)}
	var out fuse.OpenOut
	if status := a.wfs.Open(nil, &in, &out); status != fuse.OK {
		a.release(ino)
		return toWinErrno(status), invalidFh
	}
	// The resolve's reference stays with the handle until Release.
	a.retainHandle(a.fileHandle, out.Fh, ino)
	return 0, out.Fh
}

func (a *winfspFS) Truncate(path string, size int64, fh uint64) int {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	defer track(opTruncate)()
	common := fuse.SetAttrInCommon{
		Valid: fuse.FATTR_SIZE,
		Size:  uint64(size),
	}
	ino, ok := a.inodeFromFh(fh)
	if ok {
		common.Valid |= fuse.FATTR_FH
		common.Fh = fh
	} else {
		var st fuse.Status
		ino, st = a.resolve(path)
		if st != fuse.OK {
			return toWinErrno(st)
		}
		defer a.release(ino)
	}
	common.InHeader = a.header(ino)
	var out fuse.AttrOut
	code := toWinErrno(a.wfs.SetAttr(nil, &fuse.SetAttrIn{SetAttrInCommon: common}, &out))
	a.forgetInodeStat(ino)
	if code == 0 {
		a.readAhead.Invalidate(ino)
	}
	return code
}

func (a *winfspFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	defer track(opRead)()
	ino, ok := a.inodeFromFh(fh)
	if !ok {
		return -cgofuse.EBADF
	}
	return a.readAhead.Read(fh, ino, buff, ofst, func(dst []byte, off int64) int {
		return a.readDirect(ino, fh, dst, off)
	})
}

// readDirect is the unbuffered WFS read used by the read-ahead cache.
func (a *winfspFS) readDirect(ino, fh uint64, buff []byte, ofst int64) int {
	in := fuse.ReadIn{
		InHeader: a.header(ino),
		Fh:       fh,
		Offset:   uint64(ofst),
		Size:     uint32(len(buff)),
	}
	res, status := a.wfs.Read(nil, &in, buff)
	if status != fuse.OK {
		return toWinErrno(status)
	}
	if res == nil {
		return 0
	}
	data, status := res.Bytes(buff)
	if status != fuse.OK {
		return toWinErrno(status)
	}
	if len(data) > 0 && &data[0] != &buff[0] {
		copy(buff, data)
	}
	return len(data)
}

func (a *winfspFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	defer track(opWrite)()
	ino, ok := a.inodeFromFh(fh)
	if !ok {
		return -cgofuse.EBADF
	}
	in := fuse.WriteIn{
		InHeader: a.header(ino),
		Fh:       fh,
		Offset:   uint64(ofst),
		Size:     uint32(len(buff)),
	}
	written, status := a.wfs.Write(nil, &in, buff)
	if status != fuse.OK {
		return toWinErrno(status)
	}
	a.readAhead.Invalidate(ino)
	return int(written)
}

func (a *winfspFS) Flush(path string, fh uint64) int {
	defer track(opFlush)()
	ino, ok := a.inodeFromFh(fh)
	if !ok {
		return 0
	}
	in := fuse.FlushIn{InHeader: a.header(ino), Fh: fh}
	return toWinErrno(a.wfs.Flush(nil, &in))
}

func (a *winfspFS) Release(path string, fh uint64) int {
	defer track(opRelease)()
	if ino, ok := a.inodeFromFh(fh); ok {
		in := fuse.ReleaseIn{InHeader: a.header(ino), Fh: fh}
		a.wfs.Release(nil, &in)
	}
	if ino := a.releaseHandle(a.fileHandle, fh); ino != 0 {
		a.readAhead.Release(fh)
		// Whatever was cached before the open predates its writes.
		a.forgetInodeStat(ino)
		// WinFsp reports the path the handle was opened with, which a
		// rename since then has changed; park under the inode's own path.
		// A writeback flush may still be draining: the cache holds the
		// reference back until it finishes.
		a.park(ino, false)
	}
	return 0
}

func (a *winfspFS) Fsync(path string, datasync bool, fh uint64) int {
	ino, ok := a.inodeFromFh(fh)
	if !ok {
		return 0
	}
	in := fuse.FsyncIn{InHeader: a.header(ino), Fh: fh}
	if datasync {
		in.FsyncFlags = 1
	}
	return toWinErrno(a.wfs.Fsync(nil, &in))
}

func (a *winfspFS) Opendir(path string) (int, uint64) {
	defer track(opOpendir)()
	ino, st := a.resolve(path)
	if st != fuse.OK {
		return toWinErrno(st), invalidFh
	}
	in := fuse.OpenIn{InHeader: a.header(ino)}
	var out fuse.OpenOut
	if status := a.wfs.OpenDir(nil, &in, &out); status != fuse.OK {
		a.release(ino)
		return toWinErrno(status), invalidFh
	}
	a.retainHandle(a.dirHandle, out.Fh, ino)
	return 0, out.Fh
}

func (a *winfspFS) Readdir(path string,
	fill func(name string, stat *cgofuse.Stat_t, ofst int64) bool,
	ofst int64,
	fh uint64) int {
	defer track(opReaddir)()

	ino := a.handleInode(a.dirHandle, fh)
	if ino == 0 {
		var st fuse.Status
		ino, st = a.resolve(path)
		if st != fuse.OK {
			return toWinErrno(st)
		}
		defer a.release(ino)
	}
	dirPath := a.fullPath(path)
	if canonicalPath, canonicalStatus := a.wfs.inodeToPath.GetPath(ino); canonicalStatus == fuse.OK {
		dirPath = util.FullPath(winFspDirectoryListingPath(string(dirPath), string(canonicalPath), a.caseSensitive))
	}

	fill(".", nil, 0)
	fill("..", nil, 0)

	a.wfs.inodeToPath.TouchDirectory(dirPath)

	var attr fuse.Attr
	var stat cgofuse.Stat_t
	var metadataError int
	listErr := a.wfs.listDirectoryForAdapter(context.Background(), dirPath, func(entry *filer.Entry) (bool, error) {
		childPath := dirPath.Child(entry.Name())
		// A listing reports inode numbers without taking references: WinFsp
		// returns none, so registering every child would keep it forever.
		childIno := a.wfs.inodeToPath.InodeForListing(childPath, entry.Crtime.Unix(), entry.Inode)
		entry = a.wfs.directoryListingEntry(dirPath, childIno, entry)
		attr = fuse.Attr{}
		a.wfs.setAttrByFilerEntry(&attr, childIno, entry)
		stat = cgofuse.Stat_t{}
		attrToStat(&attr, &stat)
		stat.Birthtim = cgofuse.Timespec{Sec: entry.Crtime.Unix(), Nsec: int64(entry.Crtime.Nanosecond())}
		if metadataError = applyWindowsFlags(&stat, entry.Extended); metadataError != 0 {
			return false, nil
		}
		if !fill(entry.Name(), &stat, 0) {
			return false, nil
		}
		return true, nil
	})
	if listErr != nil {
		glog.Errorf("winfsp readdir list %s: %v", dirPath, listErr)
		return -cgofuse.EIO
	}
	return metadataError
}

func (a *winfspFS) Releasedir(path string, fh uint64) int {
	defer track(opReleasedir)()
	if fh != invalidFh {
		a.wfs.ReleaseDir(&fuse.ReleaseIn{Fh: fh})
	}
	if ino := a.releaseHandle(a.dirHandle, fh); ino != 0 {
		a.park(ino, true)
	}
	return 0
}

func (a *winfspFS) Fsyncdir(path string, datasync bool, fh uint64) int {
	return 0
}

func (a *winfspFS) Setxattr(path string, name string, value []byte, flags int) int {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	ino, st := a.resolve(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	defer a.release(ino)
	// cgofuse and the raw filesystem number XATTR_CREATE and XATTR_REPLACE
	// the same; TestXattrFlagValues pins both sides.
	in := &fuse.SetXAttrIn{InHeader: a.header(ino), Flags: uint32(flags), Size: uint32(len(value))}
	status := a.wfs.SetXAttr(nil, in, name, value)
	a.forgetInodeStat(ino)
	return toWinErrno(status)
}

// xattrBufferSize is the largest attribute value or name listing one reply
// carries; MAX_XATTR_VALUE_SIZE bounds what SetXAttr accepts.
const xattrBufferSize = 64 * 1024

func (a *winfspFS) Getxattr(path string, name string) (int, []byte) {
	ino, st := a.resolve(path)
	if st != fuse.OK {
		return toWinErrno(st), nil
	}
	defer a.release(ino)
	header := a.header(ino)
	dest := make([]byte, xattrBufferSize)
	size, st := a.wfs.GetXAttr(nil, &header, name, dest)
	if st != fuse.OK {
		return toWinErrno(st), nil
	}
	return 0, dest[:size]
}

func (a *winfspFS) Removexattr(path string, name string) int {
	if a.readOnly {
		return -cgofuse.EROFS
	}
	ino, st := a.resolve(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	defer a.release(ino)
	header := a.header(ino)
	status := a.wfs.RemoveXAttr(nil, &header, name)
	a.forgetInodeStat(ino)
	return toWinErrno(status)
}

func (a *winfspFS) Listxattr(path string, fill func(name string) bool) int {
	ino, st := a.resolve(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	defer a.release(ino)
	header := a.header(ino)
	dest := make([]byte, xattrBufferSize)
	size, st := a.wfs.ListXAttr(nil, &header, dest)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	for _, name := range bytes.Split(dest[:size], []byte{0}) {
		if len(name) > 0 && !fill(string(name)) {
			break
		}
	}
	return 0
}
