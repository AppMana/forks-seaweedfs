package shell

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle_map"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

// One volume whose needle metadata cannot be read must not stop the purge of
// every other volume: its own orphans stay, the others are deleted, and the
// run still fails so the incomplete volume is investigated.
func TestFsckPurgesOtherVolumesWhenOneScanIsIncomplete(t *testing.T) {
	tempFolder := t.TempDir()
	const dataNodeId = "dn1"
	const brokenVolume, healthyVolume = uint32(7), uint32(8)
	for _, vid := range []uint32{brokenVolume, healthyVolume} {
		db := needle_map.NewMemDb()
		for key := uint64(1); key <= 2; key++ {
			if err := db.Set(types.NeedleId(key), types.ToOffset(int64(key*8)), types.Size(100)); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.SaveToIdx(getVolumeFileIdFile(tempFolder, dataNodeId, vid)); err != nil {
			t.Fatal(err)
		}
		db.Close()
		// no filer references: every needle is a candidate orphan
		if err := writeToFile(nil, getFilerFileIdFile(tempFolder, vid)); err != nil {
			t.Fatal(err)
		}
	}

	server := pb.ServerAddress("dn1:8080")
	deleted := map[uint32][]string{}
	verbose, forcePurging, verifyNeedle := false, false, false
	var out bytes.Buffer
	c := &commandVolumeFsck{
		writer:       &out,
		tempFolder:   tempFolder,
		verbose:      &verbose,
		forcePurging: &forcePurging,
		verifyNeedle: &verifyNeedle,
		readNeedleMeta: func(_ pb.ServerAddress, vid uint32, _ needle_map.NeedleValue) (uint64, error) {
			if vid == brokenVolume {
				return 0, errors.New("read needle meta: unreadable needle")
			}
			return 1000, nil
		},
		volumeServers: func(uint32) ([]pb.ServerAddress, bool) { return []pb.ServerAddress{server}, true },
		deleteFileIds: func(_ pb.ServerAddress, fileIds []string) []*volume_server_pb.DeleteResult {
			var results []*volume_server_pb.DeleteResult
			for _, fid := range fileIds {
				vid := brokenVolume
				if strings.HasPrefix(fid, "8,") {
					vid = healthyVolume
				}
				deleted[vid] = append(deleted[vid], fid)
				results = append(results, &volume_server_pb.DeleteResult{FileId: fid, Status: 202})
			}
			return results
		},
	}
	volumes := map[string]map[uint32]VInfo{dataNodeId: {
		brokenVolume:  {server: server, collection: "b"},
		healthyVolume: {server: server, collection: "b"},
	}}

	err := c.findExtraChunksInVolumeServers(volumes, true, 0, 5000, 100)
	if err == nil || !strings.Contains(err.Error(), "7") {
		t.Fatalf("an incomplete volume must still fail the run and be named: %v\n%s", err, out.String())
	}
	if len(deleted[healthyVolume]) != 2 {
		t.Fatalf("healthy volume orphans purged %v, want 2\n%s", deleted[healthyVolume], out.String())
	}
	if len(deleted[brokenVolume]) != 0 {
		t.Fatalf("incomplete volume must not be purged, deleted %v", deleted[brokenVolume])
	}
}
