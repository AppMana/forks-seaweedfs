package weed_server

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// relayingPeerFiler is a peer filer with its own store: it names its position
// for a directory and, to a filer following its log, reports itself delivered
// through deliveredNs.
type relayingPeerFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	filerId     int32
	positionNs  int64
	deliveredNs int64
}

func (s *relayingPeerFiler) GetFilerConfiguration(ctx context.Context, req *filer_pb.GetFilerConfigurationRequest) (*filer_pb.GetFilerConfigurationResponse, error) {
	return &filer_pb.GetFilerConfigurationResponse{Signature: 77}, nil
}

func (s *relayingPeerFiler) TraverseBfsMetadata(req *filer_pb.TraverseBfsMetadataRequest, stream filer_pb.SeaweedFiler_TraverseBfsMetadataServer) error {
	return nil
}

func (s *relayingPeerFiler) SubscribeLocalMetadata(req *filer_pb.SubscribeMetadataRequest, stream filer_pb.SeaweedFiler_SubscribeLocalMetadataServer) error {
	if err := stream.Send(&filer_pb.SubscribeMetadataResponse{TsNs: s.deliveredNs}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

func (s *relayingPeerFiler) DirectoryChangePosition(ctx context.Context, req *filer_pb.DirectoryChangePositionRequest) (*filer_pb.DirectoryChangePositionResponse, error) {
	return &filer_pb.DirectoryChangePositionResponse{Filers: []*filer_pb.FilerDirectoryPosition{
		{FilerId: s.filerId, PositionNs: s.positionNs, Remembered: true, ListedThroughNs: s.positionNs},
	}}, nil
}

// startRelayingPeer serves fake as a peer filer and returns its address.
func startRelayingPeer(t *testing.T, fake *relayingPeerFiler) pb.ServerAddress {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := pb.NewGrpcServer()
	filer_pb.RegisterSeaweedFilerServer(server, fake)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	return pb.NewServerAddressWithGrpcPort("127.0.0.1:1", listener.Addr().(*net.TCPAddr).Port)
}

// newFilerFollowingPeer is a filer over store whose aggregator follows peer.
func newFilerFollowingPeer(t *testing.T, store *renameTestStore, peer pb.ServerAddress) *filer.Filer {
	t.Helper()
	f := newRenameTestFiler(t, store)
	f.Signature, f.UniqueFilerId = 42, 1234
	f.MetaAggregator = filer.NewMetaAggregator(f, pb.ServerAddress("127.0.0.1:2"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	f.MetaAggregator.OnPeerUpdate(&master_pb.ClusterNodeUpdate{Address: string(peer), IsAdd: true}, time.Now())
	t.Cleanup(f.MetaAggregator.Shutdown)
	return f
}

// A filer's listing holds its own changes through the position it names, but
// a peer's only through what it has replayed of the peer's log, which trails
// the position the peer names while the peer's newest change is still being
// relayed. The answer says so per filer, or a mount takes a listing lacking
// the peer's change as current through it.
func TestDirectoryChangePositionNamesWhatThisFilersListingHoldsOfEachPeer(t *testing.T) {
	peerPosition := time.Now().UnixNano()
	peerFake := &relayingPeerFiler{filerId: 22, positionNs: peerPosition, deliveredNs: peerPosition - 1}
	f := newFilerFollowingPeer(t, newRenameTestStore(), startRelayingPeer(t, peerFake))
	dialOption := grpc.WithTransportCredentials(insecure.NewCredentials())
	f.NotifyUpdateEvent(context.Background(), nil, &filer.Entry{FullPath: "/ckpt/mp_rank_00_model_states.pt"}, false, false, []int32{7})
	ownPosition, _ := f.DirectoryWriterPosition("/ckpt", 0)
	fs := &FilerServer{filer: f, grpcDialOption: dialOption}

	var got map[int32]*filer_pb.FilerDirectoryPosition
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := fs.DirectoryChangePosition(context.Background(), &filer_pb.DirectoryChangePositionRequest{Directory: "/ckpt"})
		if err != nil {
			t.Fatal(err)
		}
		got = map[int32]*filer_pb.FilerDirectoryPosition{}
		for _, p := range resp.Filers {
			got[p.FilerId] = p
		}
		if p := got[22]; p != nil && p.ListedThroughNs == peerFake.deliveredNs {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("filer B listed through %+v, want what this filer replayed of it, %d", got[22], peerFake.deliveredNs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p := got[22]; p.PositionNs != peerPosition {
		t.Fatalf("filer B at %d, want its own position %d", p.PositionNs, peerPosition)
	}
	if p := got[f.UniqueFilerId]; p == nil || p.PositionNs != ownPosition || p.ListedThroughNs != ownPosition {
		t.Fatalf("this filer: %+v, want listed through its own position %d", p, ownPosition)
	}
}

// A lookup's LogTsNs claims every event at or below it is reflected in the
// answer. A peer's change reaches this filer's store only when its aggregator
// replays it, under the peer's stamp, so the fence stays at or below what
// every peer has been replayed through; otherwise a mount fences the peer's
// event out of the entry it read.
func TestLookupFenceStaysAtOrBelowWhatPeersAreReplayedThrough(t *testing.T) {
	peerFake := &relayingPeerFiler{filerId: 22, deliveredNs: time.Now().UnixNano()}
	store := newRenameTestStore()
	store.entries["/ckpt/mp_rank_00_model_states.pt"] = newFileEntry("/ckpt/mp_rank_00_model_states.pt", 1)
	f := newFilerFollowingPeer(t, store, startRelayingPeer(t, peerFake))
	fs := &FilerServer{filer: f, entryLockTable: util.NewLockTable[util.FullPath]()}

	deadline := time.Now().Add(10 * time.Second)
	for f.MetaAggregator.PeerLowWatermarkTsNs() != peerFake.deliveredNs {
		if time.Now().After(deadline) {
			t.Fatalf("peer replayed through %d, want %d", f.MetaAggregator.PeerLowWatermarkTsNs(), peerFake.deliveredNs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, name := range []string{"mp_rank_00_model_states.pt", "mp_rank_01_model_states.pt"} {
		resp, _ := fs.LookupDirectoryEntry(context.Background(), &filer_pb.LookupDirectoryEntryRequest{Directory: "/ckpt", Name: name})
		if resp == nil {
			t.Fatalf("lookup %s: no response", name)
		}
		if resp.LogTsNs > peerFake.deliveredNs {
			t.Fatalf("lookup %s fenced at %d, past %d, what the peer's changes are replayed through", name, resp.LogTsNs, peerFake.deliveredNs)
		}
	}
}
