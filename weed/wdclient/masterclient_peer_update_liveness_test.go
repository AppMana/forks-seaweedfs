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

// This test models the other half of the 2026-08-24 Harbor registry outage on
// appmana-cluster-03.
//
// seaweedfs-master-2's masterClient stopped applying volume location updates at
// 2026-08-22 17:08:32 and never resumed. For the following 2 days 4 hours it
// logged nothing at all, while seaweedfs-master-0 logged both incremental
// updateVidMap lines and a keepalive-driven reconnect every 20-35 minutes. Its
// vidMap froze holding volumes up to 1333; volumes 1334-1341, created from
// 2026-08-23 18:41 onwards, were unknown to it. Raft was unaffected the whole
// time - master-2 kept applying "max volume id 1333 ==> 1334" log entries.
//
// A follower answers LookupVolume out of that local vidMap, so master-2 replied
// "volume id 1335 not found" with zero locations. Volume servers are seeded with
// a single master address - the Kubernetes Service seaweedfs-master:9333 - so
// every lookup they make is load balanced across all three master pods and
// roughly one in three landed on the stale one. GetWritableRemoteReplications
// then rejected the write:
//
//	replicating operations [0] is less than volume 1335 replication copy count [3]
//
// which the volume server returns as HTTP 500. A one-chunk blob usually got
// through; a 12 GB image layer is thousands of chunk writes and could not.
//
// The last line master-2 ever logged was masterclient.go:314, emitted
// IMMEDIATELY BEFORE mc.OnPeerUpdate(update, time.Now()) - the removal of
// master-2 itself from the cluster node list. The receive loop entered the
// callback and never came back. gRPC keepalive cannot rescue that: keepalive
// tears down a dead transport, but this goroutine is blocked in application
// code between two stream.Recv() calls, so nothing ever observes the failure
// and the outer reconnect loop is never reached.
//
// The invariant: a slow or stuck OnPeerUpdate callback must not be able to stop
// the masterClient from applying volume location updates. Today it can, because
// the callback is invoked inline on the receive path.

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

	// Initial seed: the follower learns volume 1333, as master-2 had.
	if err := stream.Send(&master_pb.KeepConnectedResponse{
		VolumeLocation: &master_pb.VolumeLocation{
			Url:       "127.0.0.1:8080",
			PublicUrl: "127.0.0.1:8080",
			NewVids:   []uint32{1333},
		},
	}); err != nil {
		return err
	}

	// The cluster node update that master-2 wedged on: its own removal.
	if err := stream.Send(&master_pb.KeepConnectedResponse{
		ClusterNodeUpdate: &master_pb.ClusterNodeUpdate{
			NodeType: "master",
			Address:  "127.0.0.1:9333",
			IsAdd:    false,
		},
	}); err != nil {
		return err
	}

	// Volume 1335 is created afterwards. This is what master-2 never learned.
	for i := 0; i < 20; i++ {
		if err := stream.Send(&master_pb.KeepConnectedResponse{
			VolumeLocation: &master_pb.VolumeLocation{
				Url:       "127.0.0.1:8080",
				PublicUrl: "127.0.0.1:8080",
				NewVids:   []uint32{1335},
			},
		}); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}

	<-stream.Context().Done()
	return nil
}

// TestStuckPeerUpdateDoesNotStallVolumeLocationUpdates reproduces the wedge.
//
// RED against the current tree: OnPeerUpdate is called inline from the receive
// loop, so a callback that blocks stops every later VolumeLocation from being
// applied and the vidMap is frozen for as long as the process lives.
func TestStuckPeerUpdateDoesNotStallVolumeLocationUpdates(t *testing.T) {
	master := startFakeMasterServer(t, &peerUpdateStallServer{})

	mc := NewMasterClient(
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		"", "master", pb.ServerAddress("127.0.0.1:0.19333"), "dc1", "",
		*pb.NewServiceDiscoveryFromMap(map[string]pb.ServerAddress{"m": master}),
	)

	// A peer-update callback that blocks. On the real master this was
	// OnPeerUpdate taking Topo.RaftServerAccessLock and, on the removal path,
	// making a blocking Ping to the peer being removed with waitForReady set.
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

	// Volume 1335 is announced repeatedly after the peer update. A healthy
	// client applies it regardless of the stuck callback.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, found := mc.GetLocations(1335); found {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	_, had1333 := mc.GetLocations(1333)
	t.Fatalf("volume 1335 was never applied while OnPeerUpdate was blocked (seed volume 1333 present: %v); "+
		"the receive loop is stuck in the callback, so the vidMap stays frozen and this follower "+
		"answers LookupVolume for 1335 with zero locations", had1333)
}
