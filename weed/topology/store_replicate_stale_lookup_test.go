package topology

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"github.com/seaweedfs/seaweedfs/weed/stats"
	"github.com/seaweedfs/seaweedfs/weed/storage"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// An unknown-volume lookup must be distinguished from a replica shortfall.
// One master's missing location data does not establish under-replication.

// notFoundMasterServer models the stale follower: zero locations plus the
// explicit not-found error the master sets alongside them.
type notFoundMasterServer struct {
	master_pb.UnimplementedSeaweedServer
}

func (s *notFoundMasterServer) LookupVolume(_ context.Context, req *master_pb.LookupVolumeRequest) (*master_pb.LookupVolumeResponse, error) {
	resp := &master_pb.LookupVolumeResponse{}
	for _, vid := range req.VolumeOrFileIds {
		resp.VolumeIdLocations = append(resp.VolumeIdLocations, &master_pb.LookupVolumeResponse_VolumeIdLocation{
			VolumeOrFileId: vid,
			Locations:      nil,
			Error:          fmt.Sprintf("volume id %s not found", vid),
		})
	}
	return resp, nil
}

func startMaster(t *testing.T, srv master_pb.SeaweedServer) pb.ServerAddress {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	master_pb.RegisterSeaweedServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.GracefulStop)
	_, port, _ := net.SplitHostPort(lis.Addr().String())
	return pb.ServerAddress(fmt.Sprintf("127.0.0.1:0.%s", port))
}

func TestNotFoundFromMasterIsNotReportedAsUnderReplication(t *testing.T) {
	dir := t.TempDir()
	store := storage.NewStore(
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		"127.0.0.1", 0, 0, "", "test-store",
		[]string{dir}, []int32{10}, []util.MinFreeSpace{{}},
		dir, storage.NeedleMapInMemory,
		[]types.DiskType{types.HardDriveType}, [][]string{nil},
		0, stats.DiskIOProbeConfig{},
	)

	const vid = needle.VolumeId(12)
	if err := store.AddVolume(vid, "registry", storage.NeedleMapInMemory, "020", "", 0,
		needle.GetCurrentVersion(), 0, types.HardDriveType, 0); err != nil {
		t.Fatalf("AddVolume: %v", err)
	}

	master := startMaster(t, &notFoundMasterServer{})
	masterFn := func(context.Context) pb.ServerAddress { return master }
	dial := grpc.WithTransportCredentials(insecure.NewCredentials())

	_, err := GetWritableRemoteReplications(store, dial, vid, masterFn)
	if err == nil {
		t.Fatal("expected an error when the master does not know the volume")
	}

	if strings.Contains(err.Error(), "replication copy count") {
		t.Fatalf("a not-found answer was reported as a replica shortfall: %v\n"+
			"the master said the volume is unknown to it, which on a three-master cluster means a "+
			"stale or wrong peer answered; reporting it as under-replication hides that and makes "+
			"every chunk write to this volume fail with HTTP 500", err)
	}
}
