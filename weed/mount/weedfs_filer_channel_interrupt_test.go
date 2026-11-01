package mount

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"google.golang.org/grpc"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

const channelProbeName = "probe"

// channelProbeFiler holds a lookup of channelProbeName until release closes,
// standing in for another caller's call in flight on the mount's shared
// channel. From the listing numbered holdListFrom on, a listing is answered
// only when its caller gives up; so is a lookup of heldName.
type channelProbeFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	entries      []*filer_pb.Entry
	holdListFrom int32
	heldName     string
	listCalls    atomic.Int32
	probeEntered chan struct{}
	held         chan struct{}
	release      chan struct{}
}

func newChannelProbeFiler(entries ...string) *channelProbeFiler {
	s := &channelProbeFiler{
		probeEntered: make(chan struct{}, 1),
		held:         make(chan struct{}, 16),
		release:      make(chan struct{}),
	}
	for _, name := range entries {
		s.entries = append(s.entries, &filer_pb.Entry{Name: name, Attributes: &filer_pb.FuseAttributes{Crtime: 1, Mtime: 1, FileMode: 0o100644, FileSize: 1}})
	}
	return s
}

func (s *channelProbeFiler) LookupDirectoryEntry(ctx context.Context, req *filer_pb.LookupDirectoryEntryRequest) (*filer_pb.LookupDirectoryEntryResponse, error) {
	switch req.Name {
	case channelProbeName:
		s.probeEntered <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case s.heldName:
		s.held <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &filer_pb.LookupDirectoryEntryResponse{Entry: &filer_pb.Entry{Name: req.Name, Attributes: &filer_pb.FuseAttributes{FileMode: 0o100644}}}, nil
}

func (s *channelProbeFiler) ListEntries(req *filer_pb.ListEntriesRequest, stream filer_pb.SeaweedFiler_ListEntriesServer) error {
	if n := s.listCalls.Add(1); s.holdListFrom != 0 && n >= s.holdListFrom {
		s.held <- struct{}{}
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	for _, e := range s.entries {
		if err := stream.Send(&filer_pb.ListEntriesResponse{Entry: e, SnapshotTsNs: 5000}); err != nil {
			return err
		}
	}
	return nil
}

// startChannelProbe puts a call in flight on wfs's shared channel to the filer
// and returns where its result arrives.
func startChannelProbe(t *testing.T, wfs *WFS, fake *channelProbeFiler) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- pb.WithGrpcClient(context.Background(), false, wfs.signature, func(conn *grpc.ClientConn) error {
			_, err := filer_pb.NewSeaweedFilerClient(conn).LookupDirectoryEntry(context.Background(), &filer_pb.LookupDirectoryEntryRequest{Directory: "/", Name: channelProbeName})
			return err
		}, wfs.option.FilerAddresses[0].ToGrpcAddress(), false, wfs.option.GrpcDialOption)
	}()
	select {
	case <-fake.probeEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("probe call never reached the filer")
	}
	return done
}

// requireChannelProbeSurvived fails if the probe ended while the filer still
// held it: the shared channel was torn down under it.
func requireChannelProbeSurvived(t *testing.T, fake *channelProbeFiler, probe <-chan error) {
	t.Helper()
	select {
	case err := <-probe:
		t.Fatalf("call in flight on the shared channel ended while still held by the filer: %v", err)
	case <-time.After(time.Second):
	}
	close(fake.release)
	if err := <-probe; err != nil {
		t.Fatalf("call in flight on the shared channel: %v", err)
	}
}

func waitHeld(t *testing.T, fake *channelProbeFiler) {
	t.Helper()
	select {
	case <-fake.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the call never reached the filer")
	}
}

func newChannelTestWFS(t *testing.T, fake *channelProbeFiler) *WFS {
	t.Helper()
	wfs := newLookupCacheTestWFS(t, 60)
	wfs.dhMap = NewDirectoryHandleToInode()
	wfs.inodeToPath.MarkChildrenCached("/")
	startFakeFiler(t, wfs, fake)
	return wfs
}

func readDirInterrupted(t *testing.T, wfs *WFS, fake *channelProbeFiler, dirInode uint64) fuse.Status {
	t.Helper()
	dhid, _ := wfs.AcquireDirectoryHandle()
	defer wfs.ReleaseDirectoryHandle(dhid)
	interrupt := make(chan struct{})
	done := make(chan fuse.Status, 1)
	go func() {
		ctx, cancel := ctxFromCancel(interrupt)
		defer cancel()
		done <- wfs.doReadDirectory(ctx, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: dirInode}, Fh: uint64(dhid), Size: 1 << 20}, &benchSink{}, false)
	}()
	waitHeld(t, fake)
	close(interrupt)
	select {
	case st := <-done:
		return st
	case <-time.After(10 * time.Second):
		t.Fatal("interrupted readdir did not return")
	}
	return fuse.OK
}

// A readdir of a directory too large to cache reads through to the filer; one
// the kernel interrupts answers EINTR and leaves the channel the mount's other
// calls share up.
func TestInterruptedReadThroughListingLeavesSharedFilerChannelUp(t *testing.T) {
	fake := newChannelProbeFiler()
	fake.holdListFrom = 1
	wfs := newChannelTestWFS(t, fake)
	dir := util.FullPath("/big")
	dirInode := wfs.inodeToPath.Lookup(dir, time.Now().Unix(), true, false, 0, true)
	wfs.inodeToPath.MarkDirectoryReadThrough(dir, time.Now())
	probe := startChannelProbe(t, wfs, fake)

	if st := readDirInterrupted(t, wfs, fake, dirInode); st != fuse.EINTR {
		t.Fatalf("interrupted read-through readdir answered %v, want EINTR", st)
	}
	requireChannelProbeSurvived(t, fake, probe)
}

// A readdir re-validating a section other mounts churned cannot vouch for the
// cached listing once the kernel interrupts it: it answers EINTR, not the
// unverified cache. The refresh it started runs on for other waiters and ends
// at its own bound, which is no failure of the shared channel either.
func TestInterruptedSectionRefreshAnswersEINTRAndLeavesSharedFilerChannelUp(t *testing.T) {
	saved := meta_cache.SectionRefreshTimeout
	meta_cache.SectionRefreshTimeout = 200 * time.Millisecond
	t.Cleanup(func() { meta_cache.SectionRefreshTimeout = saved })

	fake := newChannelProbeFiler("a", "b", "c")
	fake.holdListFrom = 2
	wfs := newChannelTestWFS(t, fake)
	dir := util.FullPath("/dir")
	dirInode := wfs.inodeToPath.Lookup(dir, time.Now().Unix(), true, false, 0, true)
	if err := meta_cache.EnsureVisited(context.Background(), wfs.metaCache, wfs, dir, 0); err != nil {
		t.Fatalf("EnsureVisited: %v", err)
	}
	for i := 0; i < 64; i++ {
		if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), &filer_pb.SubscribeMetadataResponse{
			Directory: string(dir),
			TsNs:      6000 + int64(i),
			EventNotification: &filer_pb.EventNotification{NewEntry: &filer_pb.Entry{
				Name: fmt.Sprintf("x-%03d", i), Attributes: &filer_pb.FuseAttributes{Crtime: 1, Mtime: 1, FileMode: 0o100644, FileSize: 1},
			}},
		}, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
			t.Fatal(err)
		}
	}
	if wfs.metaCache.IsNameFresh(dir.Child("a")) {
		t.Fatal("section should be stale after the change burst")
	}
	probe := startChannelProbe(t, wfs, fake)

	if st := readDirInterrupted(t, wfs, fake, dirInode); st != fuse.EINTR {
		t.Fatalf("interrupted readdir answered %v from a listing it could not re-validate, want EINTR", st)
	}
	time.Sleep(2 * meta_cache.SectionRefreshTimeout)
	requireChannelProbeSurvived(t, fake, probe)
}

// A path-based adapter listing a directory too large to cache reads through;
// a listing its caller abandons leaves the shared channel up.
func TestAbandonedAdapterReadThroughListingLeavesSharedFilerChannelUp(t *testing.T) {
	fake := newChannelProbeFiler("a", "b", "c")
	fake.holdListFrom = 2
	wfs := newChannelTestWFS(t, fake)
	wfs.option.CacheDirMaxEntries = 1
	dir := util.FullPath("/big")
	wfs.inodeToPath.Lookup(dir, time.Now().Unix(), true, false, 0, true)
	probe := startChannelProbe(t, wfs, fake)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- wfs.listDirectoryForAdapter(ctx, dir, func(*filer.Entry) (bool, error) { return true, nil })
	}()
	waitHeld(t, fake)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("abandoned listing reported success")
	}
	requireChannelProbeSurvived(t, fake, probe)
}

// Entry reads through the filer helpers (the copy_file_range readback among
// them) run under their caller's context; one the caller abandons leaves the
// shared channel up.
func TestAbandonedEntryReadLeavesSharedFilerChannelUp(t *testing.T) {
	fake := newChannelProbeFiler()
	fake.heldName = "copied.bin"
	wfs := newChannelTestWFS(t, fake)
	probe := startChannelProbe(t, wfs, fake)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, _, err := filer_pb.GetEntry(ctx, wfs, util.FullPath("/dst/copied.bin"))
		done <- err
	}()
	waitHeld(t, fake)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("abandoned entry read reported success")
	}
	requireChannelProbeSurvived(t, fake, probe)
}
