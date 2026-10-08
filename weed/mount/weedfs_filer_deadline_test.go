package mount

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// silentFiler accepts every request and never answers, like a filer stuck on
// its store or a node that went away without closing connections.
type silentFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	stop chan struct{}
}

func (s *silentFiler) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
	case <-s.stop:
	}
	return ctx.Err()
}

func (s *silentFiler) StreamMutateEntry(stream filer_pb.SeaweedFiler_StreamMutateEntryServer) error {
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()
	return s.wait(stream.Context())
}
func (s *silentFiler) CreateEntry(ctx context.Context, _ *filer_pb.CreateEntryRequest) (*filer_pb.CreateEntryResponse, error) {
	return nil, s.wait(ctx)
}
func (s *silentFiler) UpdateEntry(ctx context.Context, _ *filer_pb.UpdateEntryRequest) (*filer_pb.UpdateEntryResponse, error) {
	return nil, s.wait(ctx)
}
func (s *silentFiler) DeleteEntry(ctx context.Context, _ *filer_pb.DeleteEntryRequest) (*filer_pb.DeleteEntryResponse, error) {
	return nil, s.wait(ctx)
}
func (s *silentFiler) StreamRenameEntry(_ *filer_pb.StreamRenameEntryRequest, stream filer_pb.SeaweedFiler_StreamRenameEntryServer) error {
	return s.wait(stream.Context())
}
func (s *silentFiler) LookupDirectoryEntry(ctx context.Context, _ *filer_pb.LookupDirectoryEntryRequest) (*filer_pb.LookupDirectoryEntryResponse, error) {
	return nil, s.wait(ctx)
}
func (s *silentFiler) ListEntries(_ *filer_pb.ListEntriesRequest, stream filer_pb.SeaweedFiler_ListEntriesServer) error {
	return s.wait(stream.Context())
}

func newSilentFilerWFS(t *testing.T) *WFS {
	t.Helper()
	wfs := newInvalidateTestWFS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &silentFiler{stop: make(chan struct{})}
	server := pb.NewGrpcServer()
	filer_pb.RegisterSeaweedFilerServer(server, fake)
	go server.Serve(listener)
	t.Cleanup(func() {
		close(fake.stop)
		server.Stop()
		_ = listener.Close()
	})
	wfs.option.FilerAddresses = []pb.ServerAddress{
		pb.NewServerAddressWithGrpcPort("127.0.0.1:1", listener.Addr().(*net.TCPAddr).Port),
	}
	wfs.streamMutate = newStreamMutateMux(wfs)
	old := filerReplyTimeout
	filerReplyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { filerReplyTimeout = old })
	return wfs
}

func withinDeadline(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s still waiting on a filer that never answers", what)
	}
}

// Every FUSE request that waits on the filer must give up: the kernel holds
// the caller's directory lock until we reply, so an unbounded wait is an
// unbounded D-state hold (appmana-008's rename, 2026-10-08) and makes every
// lock-taking notification for that directory wait behind it.
func TestFilerRequestsGiveUpOnASilentFiler(t *testing.T) {
	wfs := newSilentFilerWFS(t)
	parent := wfs.inodeToPath.Lookup(util.FullPath("/dir"), time.Now().Unix(), true, false, 0, true)

	withinDeadline(t, "create", func() {
		if _, err := wfs.streamCreateEntry(context.Background(), &filer_pb.CreateEntryRequest{Directory: "/dir", Entry: &filer_pb.Entry{Name: "f"}}); err == nil {
			t.Error("create succeeded against a silent filer")
		}
	})
	withinDeadline(t, "update", func() {
		if _, err := wfs.streamUpdateEntry(context.Background(), &filer_pb.UpdateEntryRequest{Directory: "/dir", Entry: &filer_pb.Entry{Name: "f"}}); err == nil {
			t.Error("update succeeded against a silent filer")
		}
	})
	withinDeadline(t, "delete", func() {
		if _, err := wfs.streamDeleteEntry(context.Background(), &filer_pb.DeleteEntryRequest{Directory: "/dir", Name: "f"}); err == nil {
			t.Error("delete succeeded against a silent filer")
		}
	})
	withinDeadline(t, "rename", func() {
		req := &filer_pb.StreamRenameEntryRequest{OldDirectory: "/dir", OldName: "a", NewDirectory: "/dir", NewName: "b"}
		if err := wfs.doRename(context.Background(), req, "/dir/a", "/dir/b"); err == nil {
			t.Error("rename succeeded against a silent filer")
		}
	})
	withinDeadline(t, "lookup", func() {
		var out fuse.EntryOut
		if status := wfs.Lookup(nil, &fuse.InHeader{NodeId: parent}, "missing", &out); status == fuse.OK {
			t.Error("lookup succeeded against a silent filer")
		}
	})
}

// slowRenameFiler answers a streamed rename with one event per interval for
// longer than filerReplyTimeout in total, then finishes.
type slowRenameFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	events   int
	interval time.Duration
}

func (s *slowRenameFiler) StreamMutateEntry(stream filer_pb.SeaweedFiler_StreamMutateEntryServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			return nil
		}
		for i := 0; i < s.events; i++ {
			time.Sleep(s.interval)
			if err := stream.Send(&filer_pb.StreamMutateEntryResponse{RequestId: req.RequestId,
				Response: &filer_pb.StreamMutateEntryResponse_RenameResponse{RenameResponse: &filer_pb.StreamRenameEntryResponse{}}}); err != nil {
				return err
			}
		}
		if err := stream.Send(&filer_pb.StreamMutateEntryResponse{RequestId: req.RequestId, IsLast: true}); err != nil {
			return err
		}
	}
}

// The rename deadline is for a filer gone quiet, not a long directory rename
// that keeps reporting progress.
func TestRenameThatKeepsProgressingIsNotCutOff(t *testing.T) {
	wfs := newInvalidateTestWFS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := pb.NewGrpcServer()
	filer_pb.RegisterSeaweedFilerServer(server, &slowRenameFiler{events: 6, interval: 150 * time.Millisecond})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	wfs.option.FilerAddresses = []pb.ServerAddress{pb.NewServerAddressWithGrpcPort("127.0.0.1:1", listener.Addr().(*net.TCPAddr).Port)}
	wfs.streamMutate = newStreamMutateMux(wfs)
	old := filerReplyTimeout
	filerReplyTimeout = 400 * time.Millisecond // shorter than the 900 ms rename, longer than each gap
	t.Cleanup(func() { filerReplyTimeout = old })
	req := &filer_pb.StreamRenameEntryRequest{OldDirectory: "/dir", OldName: "a", NewDirectory: "/dir", NewName: "b"}
	if err := wfs.streamMutate.Rename(context.Background(), req, func(*filer_pb.StreamRenameEntryResponse) error { return nil }); err != nil {
		t.Fatalf("progressing rename was cut off: %v", err)
	}
}
