package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/seaweedfs/seaweedfs/weed/stats"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func writeReplaceTestVolume(t *testing.T, dir string, vid needle.VolumeId, data string) {
	t.Helper()
	v, err := NewVolume(dir, dir, "", vid, NeedleMapInMemory,
		&super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.GetCurrentVersion(), 0, 0)
	require.NoError(t, err)
	n := &needle.Needle{Id: 1, Cookie: 7, Data: []byte(data)}
	n.Checksum = needle.NewCRC(n.Data)
	_, _, _, err = v.writeNeedle2(n, true, false, false)
	require.NoError(t, err)
	v.Close()
}

func TestReplaceVolumeRestoresOldReplicaWhenMountFails(t *testing.T) {
	dir := t.TempDir()
	const vid = needle.VolumeId(21)
	writeReplaceTestVolume(t, dir, vid, "old replica")

	store := NewStore(nil, "localhost", 8080, 18080, "http://localhost:8080", "store-id",
		[]string{dir}, []int32{100}, []util.MinFreeSpace{{}}, "",
		NeedleMapInMemory, []types.DiskType{types.HardDriveType}, nil, 3,
		stats.DefaultDiskIOProbeConfig())
	t.Cleanup(store.Close)
	require.NotNil(t, store.GetVolume(vid))

	staged := StagedVolumeFileName(dir, "", int(vid))
	require.NoError(t, os.MkdirAll(filepath.Dir(staged), 0755))
	writeReplaceTestVolume(t, filepath.Dir(staged), vid, "new copy")

	validationErr := errors.New("copied volume is invalid")
	err := store.ReplaceVolume(vid, "", store.Locations[0], func(*Volume) error { return validationErr })
	require.ErrorIs(t, err, validationErr)

	n := &needle.Needle{Id: 1, Cookie: 7}
	_, err = store.ReadVolumeNeedle(vid, n, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "old replica", string(n.Data))
	require.NoDirExists(t, filepath.Dir(replacedVolumeFileName(VolumeFileName(dir, "", int(vid)))))
}
