package mount

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"google.golang.org/grpc"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// heldPositionFiler answers position requests at once until held is set;
// from then on a position request is answered only when its caller gives up.
// A lookup on it is held until release closes.
type heldPositionFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	held          atomic.Bool
	positionAsked chan struct{}
	lookupEntered chan struct{}
	release       chan struct{}
}

func newHeldPositionFiler() *heldPositionFiler {
	return &heldPositionFiler{
		positionAsked: make(chan struct{}, 1),
		lookupEntered: make(chan struct{}, 1),
		release:       make(chan struct{}),
	}
}

func (s *heldPositionFiler) DirectoryChangePosition(ctx context.Context, req *filer_pb.DirectoryChangePositionRequest) (*filer_pb.DirectoryChangePositionResponse, error) {
	if !s.held.Load() {
		return &filer_pb.DirectoryChangePositionResponse{Filers: []*filer_pb.FilerDirectoryPosition{{FilerId: 11}}}, nil
	}
	s.positionAsked <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *heldPositionFiler) ListEntries(req *filer_pb.ListEntriesRequest, stream filer_pb.SeaweedFiler_ListEntriesServer) error {
	return stream.Send(&filer_pb.ListEntriesResponse{Entry: &filer_pb.Entry{Name: "seed", Attributes: &filer_pb.FuseAttributes{FileMode: 0o644}}})
}

func (s *heldPositionFiler) LookupDirectoryEntry(ctx context.Context, req *filer_pb.LookupDirectoryEntryRequest) (*filer_pb.LookupDirectoryEntryResponse, error) {
	s.lookupEntered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &filer_pb.LookupDirectoryEntryResponse{Entry: &filer_pb.Entry{Name: req.Name, Attributes: &filer_pb.FuseAttributes{FileMode: 0o644}}}, nil
}

// A lookup the kernel interrupts while the filer is still naming the
// directory's position cannot tell whether the cached listing is current, so
// it must not answer from it: the name may be a file another mount has just
// closed. It answers EINTR, which the kernel turns into a restart, instead of
// an ENOENT the caller would believe. Go programs interrupt their own blocking
// calls whenever the runtime preempts a goroutine, so this is routine.
func TestNegativeLookupInterruptedWhileAskingThePositionAnswersEINTR(t *testing.T) {
	dir := util.FullPath("/checkpoints")
	wfs := newBenchWFS(t, dir, 4)
	fake := newHeldPositionFiler()
	startFakeFiler(t, wfs, fake)
	dirInode, _ := wfs.inodeToPath.GetInode(dir)
	openDir(t, wfs, dirInode)

	fake.held.Store(true)
	interrupt := make(chan struct{})
	done := make(chan fuse.Status, 1)
	go func() {
		var out fuse.EntryOut
		done <- wfs.Lookup(interrupt, &fuse.InHeader{NodeId: dirInode}, "r11_stage3.pt", &out)
	}()
	select {
	case <-fake.positionAsked:
	case <-time.After(10 * time.Second):
		t.Fatal("lookup never asked the filer for the directory's position")
	}
	close(interrupt)
	select {
	case st := <-done:
		if st != fuse.EINTR {
			t.Fatalf("interrupted lookup answered %v, want EINTR", st)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interrupted lookup did not return")
	}
}

// An interrupted position request is that request's own business: the other
// calls sharing the mount's channel to the filer must carry on, not fail with
// "the client connection is closing" because the channel was torn down.
func TestInterruptedPositionRequestLeavesSharedFilerChannelUp(t *testing.T) {
	dir := util.FullPath("/checkpoints")
	wfs := newBenchWFS(t, dir, 4)
	fake := newHeldPositionFiler()
	startFakeFiler(t, wfs, fake)
	dirInode, _ := wfs.inodeToPath.GetInode(dir)
	openDir(t, wfs, dirInode)

	// another caller's lookup, in flight on the shared channel
	lookupDone := make(chan error, 1)
	go func() {
		lookupDone <- pb.WithGrpcClient(context.Background(), false, wfs.signature, func(conn *grpc.ClientConn) error {
			_, err := filer_pb.NewSeaweedFilerClient(conn).LookupDirectoryEntry(context.Background(), &filer_pb.LookupDirectoryEntryRequest{Directory: string(dir), Name: "image-00000000.jpg"})
			return err
		}, wfs.option.FilerAddresses[0].ToGrpcAddress(), false, wfs.option.GrpcDialOption)
	}()
	select {
	case <-fake.lookupEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent lookup never reached the filer")
	}

	fake.held.Store(true)
	interrupt := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var out fuse.EntryOut
		wfs.Lookup(interrupt, &fuse.InHeader{NodeId: dirInode}, "r11_stage3.pt", &out)
	}()
	select {
	case <-fake.positionAsked:
	case <-time.After(10 * time.Second):
		t.Fatal("lookup never asked the filer for the directory's position")
	}
	close(interrupt)
	<-done

	select {
	case err := <-lookupDone:
		t.Fatalf("concurrent lookup ended while still held by the filer: %v", err)
	case <-time.After(time.Second):
	}
	close(fake.release)
	if err := <-lookupDone; err != nil {
		t.Fatalf("concurrent lookup: %v", err)
	}
}
