package shell

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle_map"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

// fsck copies a volume's index and then asks the volume server for each
// candidate orphan's append time at the copied offset. A vacuum that commits
// in between rewrites the volume, so the copied offsets go stale and the
// read fails for that needle. The scan is incomplete: do not label arbitrary
// RPC/disk failures as a confirmed vacuum race, or permit a subsequent purge.
func TestFsckRejectsIncompleteNeedleMetadata(t *testing.T) {
	tempFolder := t.TempDir()
	const dataNodeId = "dn1"
	const volumeId = uint32(7)

	db := needle_map.NewMemDb()
	for key := uint64(1); key <= 3; key++ {
		if err := db.Set(types.NeedleId(key), types.ToOffset(int64(key*8)), types.Size(100)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SaveToIdx(getVolumeFileIdFile(tempFolder, dataNodeId, volumeId)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	// The filer references needle 1 only; needles 2 and 3 are candidates.
	var fid bytes.Buffer
	path := "/buckets/b/one"
	var header [16]byte
	binary.BigEndian.PutUint64(header[0:8], 1)
	binary.BigEndian.PutUint32(header[8:12], 0)
	binary.BigEndian.PutUint32(header[12:16], uint32(len(path)))
	fid.Write(header[:])
	fid.WriteString(path)
	if err := os.WriteFile(getFilerFileIdFile(tempFolder, volumeId), fid.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	verbose, forcePurging, verifyNeedle := true, false, false
	var out bytes.Buffer
	c := &commandVolumeFsck{
		writer:       &out,
		tempFolder:   tempFolder,
		verbose:      &verbose,
		forcePurging: &forcePurging,
		verifyNeedle: &verifyNeedle,
		readNeedleMeta: func(server pb.ServerAddress, vid uint32, n needle_map.NeedleValue) (uint64, error) {
			if n.Key == 3 {
				return 0, errors.New("read needle meta: index out of range, volume was compacted")
			}
			return 1000, nil
		},
	}
	vinfo := VInfo{server: pb.ServerAddress("dn1:8080"), collection: "b"}

	inUse, orphans, orphanBytes, err := c.oneVolumeFileIdsSubtractFilerFileIds(dataNodeId, volumeId, &vinfo, 0, 5000)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete metadata must fail closed: %v", err)
	}
	if inUse != 1 {
		t.Fatalf("in use %d, want 1", inUse)
	}
	if len(orphans) != 0 || orphanBytes != 0 {
		t.Fatalf("incomplete scan returned actionable orphans %v (%d bytes)", orphans, orphanBytes)
	}
	if !strings.Contains(out.String(), "1 needle") || !strings.Contains(out.String(), "unknown") {
		t.Fatalf("the skipped needle must be reported, got:\n%s", out.String())
	}
}
