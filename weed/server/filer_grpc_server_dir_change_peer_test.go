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

// A filer's listing holds its own changes through the position it names, but
// a peer's only through what it has replayed of the peer's log, which trails
// the position the peer names while the peer's newest change is still being
// relayed. The answer says so per filer, or a mount takes a listing lacking
// the peer's change as current through it.
func TestDirectoryChangePositionNamesWhatThisFilersListingHoldsOfEachPeer(t *testing.T) {
	peerPosition := time.Now().UnixNano()
	peerFake := &relayingPeerFiler{filerId: 22, positionNs: peerPosition, deliveredNs: peerPosition - 1}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	peerServer := pb.NewGrpcServer()
	filer_pb.RegisterSeaweedFilerServer(peerServer, peerFake)
	go peerServer.Serve(listener)
	t.Cleanup(peerServer.Stop)
	peer := pb.NewServerAddressWithGrpcPort("127.0.0.1:1", listener.Addr().(*net.TCPAddr).Port)

	dialOption := grpc.WithTransportCredentials(insecure.NewCredentials())
	f := newRenameTestFiler(t, newRenameTestStore())
	f.Signature, f.UniqueFilerId = 42, 1234
	self := pb.ServerAddress("127.0.0.1:2")
	f.MetaAggregator = filer.NewMetaAggregator(f, self, dialOption)
	f.MetaAggregator.OnPeerUpdate(&master_pb.ClusterNodeUpdate{Address: string(peer), IsAdd: true}, time.Now())
	t.Cleanup(f.MetaAggregator.Shutdown)
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
