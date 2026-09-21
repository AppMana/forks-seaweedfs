package topology

import (
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/storage"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

func TestReplicaSizeAccountingDuringRollingCompaction(t *testing.T) {
	rp, _ := super_block.NewReplicaPlacementFromString("020")
	vl := NewVolumeLayout(rp, needle.EMPTY_TTL, types.HardDriveType, 10000, false)
	nodes := []*DataNode{NewDataNode("a"), NewDataNode("b"), NewDataNode("c")}
	for _, dn := range nodes {
		dn.Ip = string(dn.Id())
		v := storage.VolumeInfo{Id: 1, Size: 9500, ReplicaPlacement: rp, Ttl: needle.EMPTY_TTL}
		dn.AddOrUpdateVolume(v)
		vl.RegisterVolume(&v, dn)
	}
	for i, dn := range nodes {
		v := storage.VolumeInfo{Id: 1, Size: 2000, CompactRevision: 1, ReplicaPlacement: rp, Ttl: needle.EMPTY_TTL}
		dn.AddOrUpdateVolume(v)
		advanceSizeTrackingClock(vl, 1, 3*time.Second)
		vl.UpdateVolumeSizeFromReplicas(1)
		want := uint64(9500)
		if i == len(nodes)-1 {
			want = 2000
		}
		if got := vl.sizeTracking[1].effectiveSize; got != want {
			t.Fatalf("after replica %d compacts: effective size %d, want %d", i, got, want)
		}
	}
	// A lagging replica in the SAME generation may never hide a larger one.
	v, _ := nodes[0].GetVolumesById(1)
	v.Size = 9700
	nodes[0].AddOrUpdateVolume(v)
	advanceSizeTrackingClock(vl, 1, 3*time.Second)
	vl.UpdateVolumeSizeFromReplicas(1)
	if got := vl.sizeTracking[1].effectiveSize; got != 9700 {
		t.Fatalf("got %d, want largest replica 9700", got)
	}
}
