package wdclient

import (
	"context"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// A blocked peer callback must not stop volume location updates.
// Transport keepalive cannot unblock a callback executing between Recv calls.

// peerUpdateStallServer streams a volume location seed, then a cluster node
// update, then a second volume location carrying a new volume. A client that
// keeps its receive path free of the peer-update callback learns the new volume;
// a client that calls the callback inline blocks forever and never sees it.
type peerUpdateStallServer struct {
	master_pb.UnimplementedSeaweedServer
}

func (s *peerUpdateStallServer) KeepConnected(stream master_pb.Seaweed_KeepConnectedServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}

	if err := stream.Send(&master_pb.KeepConnectedResponse{
		VolumeLocation: &master_pb.VolumeLocation{
			Url:       "127.0.0.1:8080",
			PublicUrl: "127.0.0.1:8080",
			NewVids:   []uint32{11},
		},
	}); err != nil {
		return err
	}

	if err := stream.Send(&master_pb.KeepConnectedResponse{
		ClusterNodeUpdate: &master_pb.ClusterNodeUpdate{
			NodeType: "master",
			Address:  "127.0.0.1:9333",
			IsAdd:    false,
		},
	}); err != nil {
		return err
	}

	for i := 0; i < 20; i++ {
		if err := stream.Send(&master_pb.KeepConnectedResponse{
			VolumeLocation: &master_pb.VolumeLocation{
				Url:       "127.0.0.1:8080",
				PublicUrl: "127.0.0.1:8080",
				NewVids:   []uint32{12},
			},
		}); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}

	<-stream.Context().Done()
	return nil
}

func TestStuckPeerUpdateDoesNotStallVolumeLocationUpdates(t *testing.T) {
	master := startFakeMasterServer(t, &peerUpdateStallServer{})

	mc := NewMasterClient(
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		"", "master", pb.ServerAddress("127.0.0.1:0.19333"), "dc1", "",
		*pb.NewServiceDiscoveryFromMap(map[string]pb.ServerAddress{"m": master}),
	)

	blocked := make(chan struct{})
	entered := make(chan struct{}, 1)
	mc.SetOnPeerUpdateFn(func(update *master_pb.ClusterNodeUpdate, startFrom time.Time) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-blocked
	})
	t.Cleanup(func() { close(blocked) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go mc.KeepConnectedToMaster(ctx)

	// The callback must actually be reached, otherwise the test proves nothing.
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the peer update callback was never invoked; the fixture did not exercise the receive path")
	}

	// Volume 12 is announced repeatedly after the peer update. A healthy
	// client applies it regardless of the stuck callback.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, found := mc.GetLocations(12); found {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	_, had11 := mc.GetLocations(11)
	t.Fatalf("volume 12 was never applied while OnPeerUpdate was blocked (seed volume 11 present: %v); "+
		"the receive loop is stuck in the callback, so the vidMap stays frozen and this follower "+
		"answers LookupVolume for 12 with zero locations", had11)
}
