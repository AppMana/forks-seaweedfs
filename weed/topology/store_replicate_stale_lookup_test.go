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

// This is the amplifier that turned the stale master read into the 2026-08-24
// Harbor outage on appmana-cluster-03.
//
// A follower master whose vidMap had frozen answered LookupVolume for volume
// 1335 with zero locations AND an explicit error, "volume id 1335 not found"
// (master_server_handlers.go findVolumeLocation sets Error and NotFound when
// the topology has no locations). GetWritableRemoteReplications reads only
// lookupErr, the gRPC transport error, and drops lookupResult.Error entirely.
// A definitive "I do not know this volume" is therefore indistinguishable from
// "this volume has lost its replicas", and the volume server reported:
//
//	replicating operations [0] is less than volume 1335 replication copy count [3]
//
// ReplicatedWrite returns on that error BEFORE the local write, so the chunk
// POST fails with HTTP 500 and Harbor's CompleteMultipartUpload surfaces
// "s3aws: InternalError". 27,298 of these were logged on volume server
// zxa1yssb between 14:00 and 20:07 UTC.
//
// The two cases need different handling and so must be distinguishable:
// a not-found answer from one master out of three is a stale or wrong-peer
// answer that should not be treated as authoritative about replica count,
// whereas a genuine shortfall of registered replicas is a real topology fact.

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

// TestNotFoundFromMasterIsNotReportedAsUnderReplication asserts the two cases
// are distinguishable.
//
// RED against the current tree: the error says "replicating operations [0] is
// less than volume 1335 replication copy count [3]", claiming a replica
// shortfall, when the master in fact said it does not know the volume.
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

	const vid = needle.VolumeId(1335)
	// ReplicaPlacement 020 is what the harbor-registry collection uses: three copies.
	if err := store.AddVolume(vid, "harbor-registry", storage.NeedleMapInMemory, "020", "", 0,
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
