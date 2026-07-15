package mount

import (
	"context"
	"math"
	"strings"

	cgofuse "github.com/winfsp/cgofuse/fuse"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// invalidFh is the cgofuse "no file handle" sentinel (~uint64(0)).
const invalidFh = ^uint64(0)

// winfspFS adapts the inode-based WFS raw handlers to cgofuse's
// path-based FileSystemInterface, served by WinFsp on Windows. All
// filesystem logic (chunk reads, dirty-page writes, rename streaming,
// meta cache) stays in WFS; this layer only translates paths to inodes
// and cgofuse structs to go-fuse structs.
type winfspFS struct {
	cgofuse.FileSystemBase
	wfs       *WFS
	readAhead *readAheadCache
}

func newWinfspFS(wfs *WFS) *winfspFS {
	return &winfspFS{wfs: wfs, readAhead: newReadAheadCache()}
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

// resolveInode resolves a cgofuse path to the WFS inode, walking
// Lookup from the root for components not yet tracked.
func (a *winfspFS) resolveInode(path string) (uint64, fuse.Status) {
	full := a.fullPath(path)
	if ino, found := a.wfs.inodeToPath.GetInode(full); found {
		return ino, fuse.OK
	}
	ino := uint64(1) // FUSE root inode
	if path == "/" || path == "" {
		return ino, fuse.OK
	}
	for _, comp := range strings.Split(strings.Trim(path, "/"), "/") {
		var out fuse.EntryOut
		if st := a.wfs.Lookup(nil, &fuse.InHeader{NodeId: ino}, comp, &out); st != fuse.OK {
			return 0, st
		}
		ino = out.NodeId
	}
	return ino, fuse.OK
}

// resolveParent splits a cgofuse path into the parent inode and the
// leaf name.
func (a *winfspFS) resolveParent(path string) (uint64, string, fuse.Status) {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return 0, "", fuse.EINVAL
	}
	dir, name := "/", trimmed
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		dir, name = "/"+trimmed[:idx], trimmed[idx+1:]
	}
	parent, st := a.resolveInode(dir)
	if st != fuse.OK {
		return 0, "", st
	}
	return parent, name, fuse.OK
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

func (a *winfspFS) Init() {}

func (a *winfspFS) Destroy() { writeWinfspStatsTrace() }

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

func (a *winfspFS) Getattr(path string, st *cgofuse.Stat_t, fh uint64) int {
	defer track(opGetattr)()
	in := fuse.GetAttrIn{}
	if ino, ok := a.inodeFromFh(fh); ok {
		in.InHeader = a.header(ino)
	} else {
		ino, status := a.resolveInode(path)
		if status != fuse.OK {
			return toWinErrno(status)
		}
		in.InHeader = a.header(ino)
	}
	var out fuse.AttrOut
	if status := a.wfs.GetAttr(nil, &in, &out); status != fuse.OK {
		return toWinErrno(status)
	}
	attrToStat(&out.Attr, st)
	return 0
}

func (a *winfspFS) Mkdir(path string, mode uint32) int {
	defer track(opMkdir)()
	parent, name, st := a.resolveParent(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	var out fuse.EntryOut
	in := fuse.MkdirIn{InHeader: a.header(parent), Mode: mode & 07777}
	return toWinErrno(a.wfs.Mkdir(nil, &in, name, &out))
}

func (a *winfspFS) Rmdir(path string) int {
	defer track(opRmdir)()
	parent, name, st := a.resolveParent(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	hdr := a.header(parent)
	return toWinErrno(a.wfs.Rmdir(nil, &hdr, name))
}

func (a *winfspFS) Unlink(path string) int {
	defer track(opUnlink)()
	parent, name, st := a.resolveParent(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	hdr := a.header(parent)
	return toWinErrno(a.wfs.Unlink(nil, &hdr, name))
}

func (a *winfspFS) Rename(oldpath string, newpath string) int {
	defer track(opRename)()
	oldParent, oldName, st := a.resolveParent(oldpath)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	newParent, newName, st := a.resolveParent(newpath)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	in := fuse.RenameIn{
		InHeader: a.header(oldParent),
		Newdir:   newParent,
		// Flags 0 == overwrite allowed, matching Windows ReplaceIfExists.
	}
	return toWinErrno(a.wfs.Rename(nil, &in, oldName, newName))
}

func (a *winfspFS) Symlink(target string, newpath string) int {
	parent, name, st := a.resolveParent(newpath)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	var out fuse.EntryOut
	hdr := a.header(parent)
	return toWinErrno(a.wfs.Symlink(nil, &hdr, target, name, &out))
}

func (a *winfspFS) Readlink(path string) (int, string) {
	ino, st := a.resolveInode(path)
	if st != fuse.OK {
		return toWinErrno(st), ""
	}
	hdr := a.header(ino)
	out, status := a.wfs.Readlink(nil, &hdr)
	if status != fuse.OK {
		return toWinErrno(status), ""
	}
	return 0, string(out)
}

func (a *winfspFS) Link(oldpath string, newpath string) int {
	oldIno, st := a.resolveInode(oldpath)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	parent, name, st := a.resolveParent(newpath)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	var out fuse.EntryOut
	in := fuse.LinkIn{InHeader: a.header(parent), Oldnodeid: oldIno}
	return toWinErrno(a.wfs.Link(nil, &in, name, &out))
}

func (a *winfspFS) Chmod(path string, mode uint32) int {
	ino, st := a.resolveInode(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	in := fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{
		InHeader: a.header(ino),
		Valid:    fuse.FATTR_MODE,
		Mode:     mode,
	}}
	var out fuse.AttrOut
	return toWinErrno(a.wfs.SetAttr(nil, &in, &out))
}

func (a *winfspFS) Chown(path string, uid uint32, gid uint32) int {
	ino, st := a.resolveInode(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	var valid uint32
	if int32(uid) != -1 {
		valid |= fuse.FATTR_UID
	}
	if int32(gid) != -1 {
		valid |= fuse.FATTR_GID
	}
	if valid == 0 {
		return 0
	}
	in := fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{
		InHeader: a.header(ino),
		Valid:    valid,
		Owner:    fuse.Owner{Uid: uid, Gid: gid},
	}}
	var out fuse.AttrOut
	return toWinErrno(a.wfs.SetAttr(nil, &in, &out))
}

func (a *winfspFS) Utimens(path string, tmsp []cgofuse.Timespec) int {
	ino, st := a.resolveInode(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}
	in := fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{
		InHeader: a.header(ino),
	}}
	if len(tmsp) >= 2 {
		in.Valid = fuse.FATTR_ATIME | fuse.FATTR_MTIME
		in.Atime, in.Atimensec = uint64(tmsp[0].Sec), uint32(tmsp[0].Nsec)
		in.Mtime, in.Mtimensec = uint64(tmsp[1].Sec), uint32(tmsp[1].Nsec)
	} else {
		now := cgofuse.Now()
		in.Valid = fuse.FATTR_ATIME | fuse.FATTR_MTIME | fuse.FATTR_ATIME_NOW | fuse.FATTR_MTIME_NOW
		in.Atime, in.Atimensec = uint64(now.Sec), uint32(now.Nsec)
		in.Mtime, in.Mtimensec = uint64(now.Sec), uint32(now.Nsec)
	}
	var out fuse.AttrOut
	return toWinErrno(a.wfs.SetAttr(nil, &in, &out))
}

func (a *winfspFS) Access(path string, mask uint32) int {
	// HostProcess mounts run as SYSTEM; enforcement is delegated to the
	// filer-side permission checks in the WFS handlers.
	return 0
}

func (a *winfspFS) Create(path string, flags int, mode uint32) (int, uint64) {
	defer track(opCreate)()
	parent, name, st := a.resolveParent(path)
	if st != fuse.OK {
		return toWinErrno(st), invalidFh
	}
	in := fuse.CreateIn{
		InHeader: a.header(parent),
		Flags:    toGoOpenFlags(flags),
		Mode:     mode & 07777,
	}
	var out fuse.CreateOut
	if status := a.wfs.Create(nil, &in, name, &out); status != fuse.OK {
		return toWinErrno(status), invalidFh
	}
	return 0, out.Fh
}

func (a *winfspFS) Open(path string, flags int) (int, uint64) {
	defer track(opOpen)()
	ino, st := a.resolveInode(path)
	if st != fuse.OK {
		return toWinErrno(st), invalidFh
	}
	in := fuse.OpenIn{InHeader: a.header(ino), Flags: toGoOpenFlags(flags)}
	var out fuse.OpenOut
	if status := a.wfs.Open(nil, &in, &out); status != fuse.OK {
		return toWinErrno(status), invalidFh
	}
	return 0, out.Fh
}

func (a *winfspFS) Truncate(path string, size int64, fh uint64) int {
	defer track(opTruncate)()
	common := fuse.SetAttrInCommon{
		Valid: fuse.FATTR_SIZE,
		Size:  uint64(size),
	}
	if ino, ok := a.inodeFromFh(fh); ok {
		common.InHeader = a.header(ino)
		common.Valid |= fuse.FATTR_FH
		common.Fh = fh
	} else {
		ino, st := a.resolveInode(path)
		if st != fuse.OK {
			return toWinErrno(st)
		}
		common.InHeader = a.header(ino)
	}
	var out fuse.AttrOut
	code := toWinErrno(a.wfs.SetAttr(nil, &fuse.SetAttrIn{SetAttrInCommon: common}, &out))
	if code == 0 {
		if ino, ok := a.inodeFromFh(fh); ok {
			a.readAhead.Invalidate(ino)
		} else if ino, st := a.resolveInode(path); st == fuse.OK {
			a.readAhead.Invalidate(ino)
		}
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
	ino, ok := a.inodeFromFh(fh)
	if !ok {
		return 0
	}
	in := fuse.ReleaseIn{InHeader: a.header(ino), Fh: fh}
	a.wfs.Release(nil, &in)
	a.readAhead.Release(fh)
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
	ino, st := a.resolveInode(path)
	if st != fuse.OK {
		return toWinErrno(st), invalidFh
	}
	in := fuse.OpenIn{InHeader: a.header(ino)}
	var out fuse.OpenOut
	if status := a.wfs.OpenDir(nil, &in, &out); status != fuse.OK {
		return toWinErrno(status), invalidFh
	}
	return 0, out.Fh
}

func (a *winfspFS) Readdir(path string,
	fill func(name string, stat *cgofuse.Stat_t, ofst int64) bool,
	ofst int64,
	fh uint64) int {
	defer track(opReaddir)()

	dirPath := a.fullPath(path)
	ino, st := a.resolveInode(path)
	if st != fuse.OK {
		return toWinErrno(st)
	}

	fill(".", nil, 0)
	fill("..", nil, 0)

	if err := meta_cache.EnsureVisited(a.wfs.metaCache, a.wfs, dirPath); err != nil {
		glog.Errorf("winfsp readdir %s: %v", dirPath, err)
		return -cgofuse.EIO
	}
	a.wfs.inodeToPath.TouchDirectory(dirPath)

	var attr fuse.Attr
	var stat cgofuse.Stat_t
	listErr := a.wfs.metaCache.ListDirectoryEntries(context.Background(), dirPath, "", false, math.MaxInt64, func(entry *filer.Entry) (bool, error) {
		childPath := dirPath.Child(entry.Name())
		childIno := a.wfs.inodeToPath.Lookup(childPath, entry.Crtime.Unix(), entry.IsDirectory(), len(entry.HardLinkId) > 0, entry.Inode, false)
		attr = fuse.Attr{}
		a.wfs.setAttrByFilerEntry(&attr, childIno, entry)
		stat = cgofuse.Stat_t{}
		attrToStat(&attr, &stat)
		if !fill(entry.Name(), &stat, 0) {
			return false, nil
		}
		return true, nil
	})
	if listErr != nil {
		glog.Errorf("winfsp readdir list %s: %v", dirPath, listErr)
		return -cgofuse.EIO
	}
	_ = ino
	return 0
}

func (a *winfspFS) Releasedir(path string, fh uint64) int {
	defer track(opReleasedir)()
	if fh != invalidFh {
		a.wfs.ReleaseDir(&fuse.ReleaseIn{Fh: fh})
	}
	return 0
}

func (a *winfspFS) Fsyncdir(path string, datasync bool, fh uint64) int {
	return 0
}

func (a *winfspFS) Setxattr(path string, name string, value []byte, flags int) int {
	return -cgofuse.ENOSYS
}

func (a *winfspFS) Getxattr(path string, name string) (int, []byte) {
	return -cgofuse.ENOSYS, nil
}

func (a *winfspFS) Removexattr(path string, name string) int {
	return -cgofuse.ENOSYS
}

func (a *winfspFS) Listxattr(path string, fill func(name string) bool) int {
	return -cgofuse.ENOSYS
}
