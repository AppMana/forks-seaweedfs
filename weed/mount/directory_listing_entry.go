package mount

import (
	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"google.golang.org/protobuf/proto"
)

// directoryListingEntry selects metadata for adapter enumeration, not file IO.
func (wfs *WFS) directoryListingEntry(dir util.FullPath, inode uint64, entry *filer.Entry) *filer.Entry {
	if fh, found := wfs.fhMap.FindFileHandle(inode); found {
		// Match GetAttr's lock order: local writes and background chunk
		// uploads can both update this entry before filer metadata is flushed.
		lock := wfs.fhLockTable.AcquireLock("directoryListingEntry", fh.fh, util.SharedLock)
		fh.entry.RLock()
		if fh.entry.Entry != nil {
			// Readdir consumes stat attributes, link count and Windows flags,
			// not chunk records or inline content. Compute size under the lock
			// (including unflushed chunks and remote size), then clone only the
			// enumeration metadata. Copying all chunks allocates proportional
			// to file fragmentation for every directory enumeration.
			current := fh.entry.Entry
			metadata := &filer_pb.Entry{
				Name: current.Name, IsDirectory: current.IsDirectory,
				Attributes: current.Attributes, Extended: current.Extended,
				HardLinkId: current.HardLinkId, HardLinkCounter: current.HardLinkCounter,
			}
			entry = filer.FromPbEntry(string(dir), proto.Clone(metadata).(*filer_pb.Entry))
			entry.FileSize = filer.FileSize(current)
		}
		fh.entry.RUnlock()
		wfs.fhLockTable.ReleaseLock(fh.fh, lock)
	}
	return entry
}
