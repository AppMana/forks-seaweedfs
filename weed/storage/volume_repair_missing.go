package storage

import (
	"errors"
	"fmt"

	"github.com/seaweedfs/seaweedfs/weed/storage/idx"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

var ErrNeedleAlreadyExists = errors.New("needle already has an index entry")

// WriteNeedleBlobIfAbsent is an explicit repair operation, not a replacement
// for ordinary replication. The caller must establish that the chunk is still
// referenced: an absent index alone cannot distinguish loss from vacuumed
// deletion. Existing live entries AND tombstones are never overwritten.
func (v *Volume) WriteNeedleBlobIfAbsent(id types.NeedleId, blob []byte, size types.Size) error {
	v.dataFileSyncLock.Lock()
	defer v.dataFileSyncLock.Unlock()
	v.dataFileAccessLock.Lock()
	defer v.dataFileAccessLock.Unlock()
	if v.nm == nil || v.DataBackend == nil {
		return fmt.Errorf("volume %d is closed", v.Id)
	}
	if v.IsReadOnly() {
		return fmt.Errorf("volume %d is read only", v.Id)
	}
	if _, exists := v.nm.Get(id); exists {
		return fmt.Errorf("%w: volume %d needle %d", ErrNeedleAlreadyExists, v.Id, id)
	}
	// LevelDB replay omits tombstones. Absence from the live map does not
	// prove absence from the authoritative index, even after a restart.
	index, _, err := v.openIndex()
	if err != nil {
		return err
	}
	defer index.Close()
	if _, err := verifyIndexFileIntegrity(index); err != nil {
		return err
	}
	if err := idx.WalkIndexFile(index, 0, func(key types.NeedleId, _ types.Offset, _ types.Size) error {
		if key == id {
			return fmt.Errorf("%w: volume %d needle %d", ErrNeedleAlreadyExists, v.Id, id)
		}
		return nil
	}); err != nil {
		return err
	}
	if size < 0 || int64(len(blob)) != needle.GetActualSize(size, v.Version()) || len(blob) < types.NeedleHeaderSize {
		return fmt.Errorf("invalid repair blob length %d for needle %d size %d", len(blob), id, size)
	}
	var decoded needle.Needle
	if err := decoded.ReadBytes(blob, 0, size, v.Version()); err != nil {
		return fmt.Errorf("invalid repair blob for needle %d: %w", id, err)
	}
	if decoded.Id != id {
		return fmt.Errorf("repair needle id %d disagrees with blob id %d", id, decoded.Id)
	}
	if err := v.doWriteNeedleBlob(id, blob, size); err != nil {
		return err
	}
	// A successful repair acknowledgement must cover the data AND its index.
	// On an uncertain sync failure preserve bytes and return the error; do not
	// truncate a potentially durable append or allow a blind retry overwrite.
	if err := v.DataBackend.Sync(); err != nil {
		v.checkReadWriteError(err)
		return err
	}
	if err := v.nm.Sync(); err != nil {
		v.checkReadWriteError(err)
		return err
	}
	return nil
}
