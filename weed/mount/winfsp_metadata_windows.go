package mount

import (
	"encoding/binary"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	cgofuse "github.com/winfsp/cgofuse/fuse"
	"google.golang.org/protobuf/proto"
)

// Keep Windows flags separate from POSIX permissions: a readonly Windows
// directory still permits child creation. The existing Extended map survives
// filer storage, replication and mixed-version mounts without a schema change.
const windowsFlagsKey = "winfsp.flags"
const windowsFlagsMask = cgofuse.UF_READONLY | cgofuse.UF_HIDDEN | cgofuse.UF_SYSTEM | cgofuse.UF_ARCHIVE

func applyWindowsFlags(st *cgofuse.Stat_t, extended map[string][]byte) int {
	value, found := extended[windowsFlagsKey]
	if !found {
		return 0 // legacy entries retain attrToStat's default
	}
	if len(value) != 4 {
		return -cgofuse.EIO // never silently discard a corrupt readonly flag
	}
	st.Flags = binary.LittleEndian.Uint32(value) & windowsFlagsMask
	return 0
}

func (a *winfspFS) Chflags(path string, flags uint32) int {
	if flags & ^uint32(windowsFlagsMask) != 0 {
		return -cgofuse.EINVAL
	}
	return a.updateWindowsMetadata(path, func(entry *filer_pb.Entry) {
		if entry.Extended == nil {
			entry.Extended = make(map[string][]byte)
		}
		value := make([]byte, 4)
		binary.LittleEndian.PutUint32(value, flags)
		entry.Extended[windowsFlagsKey] = value // store zero too: ARCHIVE was cleared
	})
}

func (a *winfspFS) Setcrtime(path string, timestamp cgofuse.Timespec) int {
	if timestamp.Nsec < 0 || timestamp.Nsec >= 1e9 {
		return -cgofuse.EINVAL
	}
	return a.updateWindowsMetadata(path, func(entry *filer_pb.Entry) {
		entry.Attributes.Crtime = timestamp.Sec
		entry.Attributes.CrtimeNs = int32(timestamp.Nsec)
	})
}

func (a *winfspFS) updateWindowsMetadata(path string, update func(*filer_pb.Entry)) int {
	ino, status := a.resolveInode(path)
	if status != fuse.OK {
		return toWinErrno(status)
	}
	fullPath, fh, entry, status := a.wfs.maybeReadEntry(ino)
	if status != fuse.OK {
		return toWinErrno(status)
	}
	if entry == nil {
		return -cgofuse.ENOENT
	}
	if fh != nil {
		// Match SetAttr's lock order and re-read after locking: asynchronous
		// uploads and subscription refreshes can replace the entry pointer.
		fh.entryLock.Lock()
		defer fh.entryLock.Unlock()
		fh.entry.Lock()
		defer fh.entry.Unlock()
		entry = fh.entry.Entry
	} else {
		// ToProtoEntry shares maps with the cache; publish a private copy.
		entry = proto.Clone(entry).(*filer_pb.Entry)
	}
	if enforced, _ := a.wfs.wormEnforcedForEntry(fullPath, entry); enforced {
		return -cgofuse.EPERM
	}
	if entry.Attributes == nil {
		entry.Attributes = &filer_pb.FuseAttributes{}
	}
	now := time.Now()
	entry.Attributes.Ctime, entry.Attributes.CtimeNs = now.Unix(), int32(now.Nanosecond())
	update(entry)
	if fh != nil {
		fh.dirtyMetadata = true // existing Flush/Fsync persists the complete entry
		return 0
	}
	if fullPath == "" {
		a.wfs.rememberRemovedDir(ino, entry)
		return 0
	}
	return toWinErrno(a.wfs.saveEntry(fullPath, entry))
}

func (a *winfspFS) getattrWindowsFlags(ino uint64, st *cgofuse.Stat_t) int {
	_, fh, entry, status := a.wfs.maybeReadEntry(ino)
	if status != fuse.OK {
		return toWinErrno(status)
	}
	if fh != nil {
		fh.entryLock.RLock()
		defer fh.entryLock.RUnlock()
		fh.entry.RLock()
		defer fh.entry.RUnlock()
		entry = fh.entry.Entry
	}
	if entry == nil {
		return -cgofuse.ENOENT
	}
	st.Birthtim = cgofuse.Timespec{Sec: entry.GetAttributes().GetCrtime(), Nsec: int64(entry.GetAttributes().GetCrtimeNs())}
	return applyWindowsFlags(st, entry.Extended)
}
