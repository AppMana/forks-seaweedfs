package topology

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type vacuumSchedulerServer struct {
	volume_server_pb.UnimplementedVolumeServerServer
	active, peak, calls atomic.Int32
}

func (s *vacuumSchedulerServer) VacuumVolumeCheck(context.Context, *volume_server_pb.VacuumVolumeCheckRequest) (*volume_server_pb.VacuumVolumeCheckResponse, error) {
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for old := s.peak.Load(); n > old; old = s.peak.Load() {
		if s.peak.CompareAndSwap(old, n) {
			break
		}
	}
	s.calls.Add(1)
	// Keep the worker occupied while the scheduler encounters its quota.
	time.Sleep(20 * time.Millisecond)
	return &volume_server_pb.VacuumVolumeCheckResponse{GarbageRatio: 0}, nil
}

func TestVacuumSchedulerWakesWhenQuotaReturns(t *testing.T) {
	testVacuumSchedulerQuota(t, 2, 1)
}

func TestVacuumSchedulerPreservesParallelQuota(t *testing.T) {
	testVacuumSchedulerQuota(t, 12, 2)
}

func testVacuumSchedulerQuota(t *testing.T, volumes, quota int) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	fake := &vacuumSchedulerServer{}
	volume_server_pb.RegisterVolumeServerServer(server, fake)
	go server.Serve(lis)
	defer server.Stop()
	dn := NewDataNode("scheduler-test")
	dn.Ip, dn.Port, dn.GrpcPort = "127.0.0.1", 8080, lis.Addr().(*net.TCPAddr).Port
	rp, _ := super_block.NewReplicaPlacementFromString("000")
	layout := NewVolumeLayout(rp, needle.EMPTY_TTL, types.HardDriveType, 10000, false)
	for vid := needle.VolumeId(1); vid <= needle.VolumeId(volumes); vid++ {
		layout.vid2location[vid] = &VolumeLocationList{list: []*DataNode{dn}}
	}
	topo := &Topology{volumeSizeLimit: 10000}
	start := time.Now()
	topo.vacuumOneVolumeLayout(grpc.WithTransportCredentials(insecure.NewCredentials()), layout, &Collection{Name: "test"}, 0.3, quota, 0, false)
	elapsed := time.Since(start)
	if fake.calls.Load() != int32(volumes) || fake.peak.Load() != int32(quota) {
		t.Fatalf("calls=%d peak=%d; require %d checks with per-server concurrency %d", fake.calls.Load(), fake.peak.Load(), volumes, quota)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("%d 20ms checks waited %v for quota; scheduler must wake on completion", volumes, elapsed)
	}
	t.Logf("%d checks with concurrency %d completed in %v", volumes, quota, elapsed)
}
