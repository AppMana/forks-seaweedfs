package needle

import (
	"fmt"
	"io"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/storage/backend"
	. "github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"github.com/seaweedfs/seaweedfs/weed/util/buffer_pool"
)

func (n *Needle) Append(w backend.BackendStorageFile, version Version) (offset uint64, size Size, actualSize int64, err error) {
	end, _, e := w.GetStat()
	if e != nil {
		err = fmt.Errorf("Cannot Read Current Volume Position: %w", e)
		return
	}
	offset = uint64(end)
	if offset >= MaxPossibleVolumeSize && len(n.Data) != 0 {
		err = fmt.Errorf("Volume Size %d Exceeded %d", offset, MaxPossibleVolumeSize)
		return
	}
	bytesBuffer := buffer_pool.SyncPoolGetBuffer()
	defer func() {
		if err != nil {
			if te := w.Truncate(end); te != nil {
				// handle error or log
			}
		}
		buffer_pool.SyncPoolPutBuffer(bytesBuffer)
	}()

	// The pooled buffer holds only the record's header and footer; the payload
	// is written straight from n.Data. Copying n.Data into the buffer made
	// every append, on the primary and on each replica, hold a second copy of
	// the needle, and sync.Pool kept the grown buffer until two collections.
	size, actualSize, dataAt, err := writeNeedleFramingByVersion(version, n, offset, bytesBuffer)
	if err != nil {
		return
	}

	// The record is written as up to three contiguous WriteAt calls. Nothing
	// else writes this file in between: a volume's appends and deletes run
	// under Volume.dataFileAccessLock (syncWrite, the async request worker,
	// deleteNeedle2), and vacuum and merge append to a destination file they
	// own. Any failed or short segment returns an error, and the deferred
	// Truncate(end) above removes every segment already written.
	framing := bytesBuffer.Bytes()
	at := int64(offset)
	for _, segment := range [][]byte{framing[:dataAt], n.Data, framing[dataAt:]} {
		if len(segment) == 0 {
			continue
		}
		var written int
		written, err = w.WriteAt(segment, at)
		if err == nil && written != len(segment) {
			err = io.ErrShortWrite
		}
		if err != nil {
			err = fmt.Errorf("failed to write %d bytes to %s at offset %d: %w", actualSize, w.Name(), offset, err)
			return offset, size, actualSize, err
		}
		at += int64(written)
	}

	return offset, size, actualSize, nil
}

func WriteNeedleBlob(w backend.BackendStorageFile, dataSlice []byte, size Size, appendAtNs uint64, version Version) (offset uint64, err error) {

	if end, _, e := w.GetStat(); e == nil {
		defer func(w backend.BackendStorageFile, off int64) {
			if err != nil {
				if te := w.Truncate(end); te != nil {
					glog.V(0).Infof("Failed to truncate %s back to %d with error: %v", w.Name(), end, te)
				}
			}
		}(w, end)
		offset = uint64(end)
	} else {
		err = fmt.Errorf("Cannot Read Current Volume Position: %v", e)
		return
	}

	if version == Version3 {
		// compute byte offset as int to compare and slice correctly
		tsOffset := int(NeedleHeaderSize) + int(size) + NeedleChecksumSize
		// Ensure dataSlice has enough capacity for the timestamp
		if tsOffset < 0 {
			err = fmt.Errorf("invalid needle size %d results in negative timestamp offset %d", size, tsOffset)
			return
		}
		if tsOffset+TimestampSize > len(dataSlice) {
			err = fmt.Errorf("needle blob buffer too small: need %d bytes, have %d", tsOffset+TimestampSize, len(dataSlice))
			return
		}
		util.Uint64toBytes(dataSlice[tsOffset:tsOffset+TimestampSize], appendAtNs)
	}

	if err == nil {
		_, err = w.WriteAt(dataSlice, int64(offset))
	}

	return

}
