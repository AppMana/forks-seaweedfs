package mount

import (
	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"google.golang.org/protobuf/proto"
)

// directoryListingEntry selects the metadata passed to adapter enumeration.
func (wfs *WFS) directoryListingEntry(dir util.FullPath, inode uint64, entry *filer.Entry) *filer.Entry {
	if fh, found := wfs.fhMap.FindFileHandle(inode); found {
		// Match GetAttr's lock order: local writes and background chunk
		// uploads can both update this entry before filer metadata is flushed.
		lock := wfs.fhLockTable.AcquireLock("directoryListingEntry", fh.fh, util.SharedLock)
		fh.entry.RLock()
		if fh.entry.Entry != nil {
			// Conversion retains protobuf slices/maps; clone before releasing
			// the locks so later writes cannot race the enumeration callback.
			entry = filer.FromPbEntry(string(dir), proto.Clone(fh.entry.Entry).(*filer_pb.Entry))
		}
		fh.entry.RUnlock()
		wfs.fhLockTable.ReleaseLock(fh.fh, lock)
	}
	return entry
}
