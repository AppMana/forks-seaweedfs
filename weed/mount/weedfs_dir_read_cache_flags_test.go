package mount

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// positionFiler reports a fixed directory change position, or fails with err.
type positionFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	tsNs       int64
	remembered bool
	filers     []*filer_pb.FilerDirectoryPosition
	signature  atomic.Int32 // the caller's, from the last request
	err        error
	listed     []*filer_pb.Entry // served by ListEntries
	calls      atomic.Int32
}

func (s *positionFiler) DirectoryChangePosition(ctx context.Context, req *filer_pb.DirectoryChangePositionRequest) (*filer_pb.DirectoryChangePositionResponse, error) {
	s.calls.Add(1)
	s.signature.Store(req.Signature)
	if s.err != nil {
		return nil, s.err
	}
	return &filer_pb.DirectoryChangePositionResponse{TsNs: s.tsNs, Remembered: s.remembered, Filers: s.filers}, nil
}

func (s *positionFiler) ListEntries(req *filer_pb.ListEntriesRequest, stream filer_pb.SeaweedFiler_ListEntriesServer) error {
	for _, entry := range s.listed {
		if err := stream.Send(&filer_pb.ListEntriesResponse{Entry: entry}); err != nil {
			return err
		}
	}
	return nil
}

func openDir(t *testing.T, wfs *WFS, inode uint64) uint32 {
	t.Helper()
	var out fuse.OpenOut
	if st := wfs.OpenDir(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: inode}}, &out); st != fuse.OK {
		t.Fatalf("OpenDir: %v", st)
	}
	wfs.ReleaseDir(&fuse.ReleaseIn{Fh: out.Fh})
	if out.OpenFlags&fuse.FOPEN_CACHE_DIR == 0 {
		t.Fatal("OpenDir reply lacks FOPEN_CACHE_DIR")
	}
	return out.OpenFlags
}

// The kernel caches listings, and an unchanged directory reopened keeps the
// listing the kernel holds: losing either would revert repeat listings to full
// walks of the mount.
func TestOpenDirKeepsKernelListingOfUnchangedDirectory(t *testing.T) {
	dir := util.FullPath("/images")
	wfs := newBenchWFS(t, dir, 4)
	startFakeFiler(t, wfs, &positionFiler{filers: []*filer_pb.FilerDirectoryPosition{{FilerId: 11}}})
	dirInode, _ := wfs.inodeToPath.GetInode(dir)

	openDir(t, wfs, dirInode)
	if openDir(t, wfs, dirInode)&fuse.FOPEN_KEEP_CACHE == 0 {
		t.Fatal("reopening an unchanged directory dropped the kernel's listing")
	}
}

// A filer naming only its newest change across filers cannot say whether a
// peer's change is still on its way, so every open reads the directory again.
func TestOpenDirRelistsWithFilerWithoutPerFilerPositions(t *testing.T) {
	dir := util.FullPath("/images")
	wfs := newBenchWFS(t, dir, 4)
	fake := &positionFiler{tsNs: 0, listed: []*filer_pb.Entry{
		{Name: "late.pt", Attributes: &filer_pb.FuseAttributes{FileMode: 0o644}},
	}}
	startFakeFiler(t, wfs, fake)
	dirInode, _ := wfs.inodeToPath.GetInode(dir)

	openDir(t, wfs, dirInode)
	if openDir(t, wfs, dirInode)&fuse.FOPEN_KEEP_CACHE != 0 {
		t.Fatal("kept a listing a filer without per-filer positions could not verify")
	}
	if entry, _, err := wfs.metaCache.FindEntry(context.Background(), dir.Child("late.pt")); err != nil || entry == nil {
		t.Fatalf("relisted directory lacks the filer's entry: %v", err)
	}
}

// A change another mount has acknowledged but whose event has not arrived yet
// holds the open until the event is applied, and the kernel does not keep a
// listing read before it.
func TestOpenDirWaitsForAcknowledgedChange(t *testing.T) {
	dir := util.FullPath("/images")
	wfs := newBenchWFS(t, dir, 4)
	fake := &positionFiler{filers: []*filer_pb.FilerDirectoryPosition{{FilerId: 11}}}
	startFakeFiler(t, wfs, fake)
	dirInode, _ := wfs.inodeToPath.GetInode(dir)
	openDir(t, wfs, dirInode)

	fake.filers = []*filer_pb.FilerDirectoryPosition{{FilerId: 11, PositionNs: 5000, Remembered: true}}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = wfs.metaCache.ApplyMetadataResponse(context.Background(), relayedCreate(dir, "mp_rank_03_model_states.pt", 11, 5000), meta_cache.SubscriberMetadataResponseApplyOptions)
	}()
	listed := fake.calls.Load()
	if openDir(t, wfs, dirInode)&fuse.FOPEN_KEEP_CACHE != 0 {
		t.Error("kernel kept a listing read before the change")
	}
	if entry, _, err := wfs.metaCache.FindEntry(context.Background(), dir.Child("mp_rank_03_model_states.pt")); err != nil || entry == nil {
		t.Fatalf("listing opened after the change was acknowledged does not hold it: %v", err)
	}
	if calls := fake.calls.Load(); calls != listed+1 {
		t.Fatalf("asked the filer %d times for one open the stream answered, want once", calls-listed)
	}
}

func relayedCreate(dir util.FullPath, name string, origin int32, positionNs int64) *filer_pb.SubscribeMetadataResponse {
	return &filer_pb.SubscribeMetadataResponse{
		Directory: string(dir),
		TsNs:      positionNs,
		EventNotification: &filer_pb.EventNotification{
			NewEntry:         &filer_pb.Entry{Name: name, Attributes: &filer_pb.FuseAttributes{FileMode: 0o644}},
			NewParentPath:    string(dir),
			OriginFilerId:    origin,
			OriginPositionNs: positionNs,
		},
	}
}

// A change the metadata stream does not bring in time is read from the filer.
func TestOpenDirRelistsWhenTheStreamDoesNotDeliver(t *testing.T) {
	saved := directoryCurrentWait
	directoryCurrentWait = 20 * time.Millisecond
	t.Cleanup(func() { directoryCurrentWait = saved })

	dir := util.FullPath("/images")
	wfs := newBenchWFS(t, dir, 4)
	fake := &positionFiler{filers: []*filer_pb.FilerDirectoryPosition{{FilerId: 11}}}
	startFakeFiler(t, wfs, fake)
	dirInode, _ := wfs.inodeToPath.GetInode(dir)
	openDir(t, wfs, dirInode)

	fake.filers = []*filer_pb.FilerDirectoryPosition{{FilerId: 11, PositionNs: 5000, Remembered: true}}
	fake.listed = []*filer_pb.Entry{{Name: "late.pt", Attributes: &filer_pb.FuseAttributes{FileMode: 0o644}}}
	openDir(t, wfs, dirInode)
	if entry, _, err := wfs.metaCache.FindEntry(context.Background(), dir.Child("late.pt")); err != nil || entry == nil {
		t.Fatalf("relisted directory lacks the filer's entry: %v", err)
	}
	if _, _, err := wfs.metaCache.FindEntry(context.Background(), dir.Child("image-00000000.jpg")); err == nil {
		t.Fatal("relisted directory kept an entry the filer no longer lists")
	}
	if _, pending := wfs.metaCache.DirectoryPending(dir, fake.filers); len(pending) != 0 {
		t.Fatalf("relisted directory not current through the positions named before it: %v", pending)
	}
}

// A position missing a peer filer's changes is not trusted: the directory is
// read from the filer that answered.
func TestOpenDirRelistsWhenAPeerFilerCannotAnswer(t *testing.T) {
	dir := util.FullPath("/images")
	wfs := newBenchWFS(t, dir, 4)
	startFakeFiler(t, wfs, &positionFiler{err: status.Error(codes.Aborted, "peer filer down"), listed: []*filer_pb.Entry{
		{Name: "late.pt", Attributes: &filer_pb.FuseAttributes{FileMode: 0o644}},
	}})
	dirInode, _ := wfs.inodeToPath.GetInode(dir)

	openDir(t, wfs, dirInode)
	if entry, _, err := wfs.metaCache.FindEntry(context.Background(), dir.Child("late.pt")); err != nil || entry == nil {
		t.Fatalf("relisted directory lacks the filer's entry: %v", err)
	}
}

// With the filer unreachable the cached listing is still served, promptly,
// but the kernel does not keep it as verified.
func TestOpenDirServesCacheWhenFilerUnreachable(t *testing.T) {
	saved := directoryPositionTimeout
	directoryPositionTimeout = 100 * time.Millisecond
	t.Cleanup(func() { directoryPositionTimeout = saved })

	dir := util.FullPath("/images")
	wfs := newBenchWFS(t, dir, 4)
	dirInode, _ := wfs.inodeToPath.GetInode(dir)

	start := time.Now()
	openDir(t, wfs, dirInode)
	if flags := openDir(t, wfs, dirInode); flags&fuse.FOPEN_KEEP_CACHE != 0 {
		t.Error("kernel kept an unverified listing")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("opens took %v with the filer unreachable", elapsed)
	}
	if !wfs.metaCache.IsDirectoryCached(dir) {
		t.Fatal("unreachable filer dropped the cached listing")
	}
}

// A filer without positions leaves listings to the metadata stream, as before.
func TestOpenDirWithFilerWithoutPositions(t *testing.T) {
	dir := util.FullPath("/images")
	wfs := newBenchWFS(t, dir, 4)
	fake := &positionFiler{err: status.Error(codes.Unimplemented, "unknown method")}
	startFakeFiler(t, wfs, fake)
	dirInode, _ := wfs.inodeToPath.GetInode(dir)

	openDir(t, wfs, dirInode)
	if openDir(t, wfs, dirInode)&fuse.FOPEN_KEEP_CACHE == 0 {
		t.Fatal("reopening an unchanged directory dropped the kernel's listing")
	}
	if calls := fake.calls.Load(); calls != 1 {
		t.Fatalf("asked a filer without positions %d times, want once", calls)
	}
}

// A lookup of a name another mount has just created, in a directory this mount
// has cached, finds it instead of answering ENOENT from the cache.
func TestNegativeLookupWaitsForAcknowledgedCreate(t *testing.T) {
	dir := util.FullPath("/images")
	wfs := newBenchWFS(t, dir, 4)
	fake := &positionFiler{filers: []*filer_pb.FilerDirectoryPosition{{FilerId: 11}}}
	startFakeFiler(t, wfs, fake)
	dirInode, _ := wfs.inodeToPath.GetInode(dir)
	openDir(t, wfs, dirInode)

	fake.filers = []*filer_pb.FilerDirectoryPosition{{FilerId: 11, PositionNs: 5000, Remembered: true}}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = wfs.metaCache.ApplyMetadataResponse(context.Background(), relayedCreate(dir, "mp_rank_03_model_states.pt", 11, 5000), meta_cache.SubscriberMetadataResponseApplyOptions)
	}()
	entry, _, st := wfs.lookupEntry(context.Background(), dir.Child("mp_rank_03_model_states.pt"))
	if st != fuse.OK || entry == nil {
		t.Fatalf("lookup of an acknowledged create: %v", st)
	}
	if _, _, st := wfs.lookupEntry(context.Background(), dir.Child("never-created")); st != fuse.ENOENT {
		t.Fatalf("lookup of an absent name: %v, want ENOENT", st)
	}
}

// A mount's own acknowledged change says nothing about a peer filer's change
// still on its way: rank 0 creates a file through filer A while rank 1's file,
// created earlier through filer B, has not been relayed to A's stream. Neither
// the newest timestamp across filers (rank 0's) nor the mount's own apply may
// pass the listing as current without rank 1's file.
func TestOpenDirWaitsForPeerFilerChangeBehindOwnChange(t *testing.T) {
	saved := directoryCurrentWait
	directoryCurrentWait = 20 * time.Millisecond
	t.Cleanup(func() { directoryCurrentWait = saved })

	const filerA, filerB = 11, 22
	dir := util.FullPath("/ckpt")
	wfs := newBenchWFS(t, dir, 0)
	images := []*filer_pb.Entry{}
	fake := &positionFiler{filers: []*filer_pb.FilerDirectoryPosition{
		{FilerId: filerA, PositionNs: 0}, {FilerId: filerB, PositionNs: 0},
	}, listed: images}
	startFakeFiler(t, wfs, fake)
	dirInode, _ := wfs.inodeToPath.GetInode(dir)
	openDir(t, wfs, dirInode)

	// rank 0's own file, acknowledged by filer A at 200 and applied locally
	own := &filer_pb.Entry{Name: "mp_rank_00_model_states.pt", Attributes: &filer_pb.FuseAttributes{FileMode: 0o644}}
	if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), &filer_pb.SubscribeMetadataResponse{
		Directory:         string(dir),
		TsNs:              200,
		EventNotification: &filer_pb.EventNotification{NewEntry: own, NewParentPath: string(dir), Signatures: []int32{wfs.signature}},
	}, meta_cache.LocalMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	peer := &filer_pb.Entry{Name: "mp_rank_01_model_states.pt", Attributes: &filer_pb.FuseAttributes{FileMode: 0o644}}
	fake.tsNs, fake.remembered = 200, true
	fake.filers = []*filer_pb.FilerDirectoryPosition{
		{FilerId: filerA, PositionNs: 0},
		{FilerId: filerB, PositionNs: 100, Remembered: true},
	}
	fake.listed = []*filer_pb.Entry{own, peer}

	openDir(t, wfs, dirInode)
	if entry, _, err := wfs.metaCache.FindEntry(context.Background(), dir.Child(peer.Name)); err != nil || entry == nil {
		t.Fatalf("listing passed as current without the peer filer's change: %v", err)
	}
	if got := fake.signature.Load(); got != wfs.signature {
		t.Fatalf("position request carried signature %d, want the mount's %d", got, wfs.signature)
	}
}
