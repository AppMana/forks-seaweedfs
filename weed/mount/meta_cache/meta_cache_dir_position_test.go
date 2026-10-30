package meta_cache

import (
	"context"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

const filerA, filerB = 11, 22

func createEventAt(dir, name string, tsNs int64) *filer_pb.SubscribeMetadataResponse {
	return &filer_pb.SubscribeMetadataResponse{
		Directory: dir,
		TsNs:      tsNs,
		EventNotification: &filer_pb.EventNotification{
			NewEntry:      &filer_pb.Entry{Name: name, Attributes: &filer_pb.FuseAttributes{FileMode: 0100644}},
			NewParentPath: dir,
		},
	}
}

// relayedEventAt is a create relayed from the filer that logged it, at its
// position in that filer's log.
func relayedEventAt(dir, name string, origin int32, positionNs int64) *filer_pb.SubscribeMetadataResponse {
	event := createEventAt(dir, name, positionNs)
	event.EventNotification.OriginFilerId = origin
	event.EventNotification.OriginPositionNs = positionNs
	return event
}

func at(filerId int32, positionNs int64, remembered bool) *filer_pb.FilerDirectoryPosition {
	return &filer_pb.FilerDirectoryPosition{FilerId: filerId, PositionNs: positionNs, Remembered: remembered}
}

// A listing is current through a filer's position once a relayed event of that
// filer at or past it is applied, and a waiter is released by its arrival.
func TestDirectoryPositionsFollowRelayedChangesPerFiler(t *testing.T) {
	mc, _, _, _ := newTestMetaCache(t, map[util.FullPath]bool{"/": true, "/dir": true})
	defer mc.Shutdown()

	want := []*filer_pb.FilerDirectoryPosition{at(filerA, 200, true)}
	if basis, _ := mc.DirectoryPending("/dir", want); basis {
		t.Fatal("a listing nobody asked the filers about has a basis")
	}
	mc.NoteDirectoryBasis("/dir", []*filer_pb.FilerDirectoryPosition{at(filerA, 0, false), at(filerB, 0, false)})

	released := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		released <- mc.AwaitDirectoryPositions(ctx, "/dir", want)
	}()

	// filer B's log says nothing about filer A's
	if err := mc.ApplyMetadataResponse(context.Background(), relayedEventAt("/dir", "b", filerB, 900), SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	if err := mc.ApplyMetadataResponse(context.Background(), relayedEventAt("/dir", "a", filerA, 100), SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
		t.Fatal("waiter for filer A at 200 released by filer A at 100 and filer B at 900")
	case <-time.After(50 * time.Millisecond):
	}

	if err := mc.ApplyMetadataResponse(context.Background(), relayedEventAt("/dir", "c", filerA, 200), SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	if !<-released {
		t.Fatal("waiter for filer A at 200 not released by its change at 200")
	}
	if entry, _, err := mc.FindEntry(context.Background(), "/dir/c"); err != nil || entry == nil {
		t.Fatalf("listing current through 200 does not hold the change at 200: %v", err)
	}
}

// The mount's own change, applied from the filer's reply, never moves a
// position: the stream never delivers the mount's own events, so the reply
// says nothing about what else the filer appended before it.
func TestLocalReplyDoesNotMovePositions(t *testing.T) {
	mc, _, _, _ := newTestMetaCache(t, map[util.FullPath]bool{"/": true, "/dir": true})
	defer mc.Shutdown()
	mc.NoteDirectoryBasis("/dir", []*filer_pb.FilerDirectoryPosition{at(filerA, 0, false)})
	generation := mc.DirectoryGeneration("/dir")

	if err := mc.ApplyMetadataResponse(context.Background(), createEventAt("/dir", "own", 500), LocalMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	if _, pending := mc.DirectoryPending("/dir", []*filer_pb.FilerDirectoryPosition{at(filerA, 1, true)}); len(pending) != 1 {
		t.Fatal("the mount's own change moved filer A's position")
	}
	if mc.DirectoryGeneration("/dir") != generation+1 {
		t.Fatalf("the mount's own create advanced generation from %d to %d, want %d", generation, mc.DirectoryGeneration("/dir"), generation+1)
	}

	update := &filer_pb.SubscribeMetadataResponse{
		Directory: "/dir",
		TsNs:      600,
		EventNotification: &filer_pb.EventNotification{
			OldEntry:      &filer_pb.Entry{Name: "own", Attributes: &filer_pb.FuseAttributes{FileMode: 0100644}},
			NewEntry:      &filer_pb.Entry{Name: "own", Attributes: &filer_pb.FuseAttributes{FileMode: 0100644, FileSize: 10}},
			NewParentPath: "/dir",
		},
	}
	if err := mc.ApplyMetadataResponse(context.Background(), update, LocalMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	if mc.DirectoryGeneration("/dir") != generation+2 {
		t.Fatalf("in-place update advanced generation to %d, want %d (once per event)", mc.DirectoryGeneration("/dir"), generation+2)
	}
}

// A rename across directories changes both parents, and a moved directory's
// own children.
func TestDirectoryPositionsCoverRenameParentsAndMovedDirectory(t *testing.T) {
	mc, _, _, _ := newTestMetaCache(t, map[util.FullPath]bool{"/": true, "/a": true, "/b": true, "/b/sub": true})
	defer mc.Shutdown()
	for _, dir := range []util.FullPath{"/a", "/b", "/b/sub"} {
		mc.NoteDirectoryBasis(dir, nil)
	}

	rename := &filer_pb.SubscribeMetadataResponse{
		Directory: "/a",
		TsNs:      300,
		EventNotification: &filer_pb.EventNotification{
			OldEntry:         &filer_pb.Entry{Name: "sub", IsDirectory: true},
			NewEntry:         &filer_pb.Entry{Name: "sub", IsDirectory: true, Attributes: &filer_pb.FuseAttributes{FileMode: 040755}},
			NewParentPath:    "/b",
			OriginFilerId:    filerA,
			OriginPositionNs: 300,
		},
	}
	if err := mc.ApplyMetadataResponse(context.Background(), rename, SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []util.FullPath{"/a", "/b", "/b/sub"} {
		if _, pending := mc.DirectoryPending(dir, []*filer_pb.FilerDirectoryPosition{at(filerA, 300, true)}); len(pending) != 0 {
			t.Errorf("%s not current through filer A at 300", dir)
		}
	}
}

// An uncached directory reads through to the filer and keeps no position.
func TestDirectoryPositionsSkipUncachedDirectories(t *testing.T) {
	mc, _, _, _ := newTestMetaCache(t, map[util.FullPath]bool{"/": true})
	defer mc.Shutdown()

	if err := mc.ApplyMetadataResponse(context.Background(), relayedEventAt("/elsewhere", "x", filerA, 100), SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	mc.NoteDirectoryBasis("/elsewhere", []*filer_pb.FilerDirectoryPosition{at(filerA, 100, true)})
	mc.RLock()
	_, found := mc.dirChangesApplied["/elsewhere"]
	mc.RUnlock()
	if found {
		t.Fatal("position recorded for an uncached directory")
	}
}

// Dropping a directory's cached listing drops its positions and basis with it:
// a rebuilt listing starts from what the filers name before it is read.
func TestDirectoryPositionsResetByPurge(t *testing.T) {
	cached := map[util.FullPath]bool{"/": true, "/dir": true}
	mc, _, _, _ := newTestMetaCache(t, cached)
	defer mc.Shutdown()
	mc.NoteDirectoryBasis("/dir", nil)

	if err := mc.ApplyMetadataResponse(context.Background(), relayedEventAt("/dir", "a", filerA, 100), SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	mc.PurgeDirectoryChildren("/dir", func() { close(done) })
	<-done
	// The purge runs on the apply loop; a later request is ordered after it.
	if err := mc.ApplyMetadataResponse(context.Background(), createEventAt("/other", "x", 1), SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	if basis, _ := mc.DirectoryPending("/dir", nil); basis {
		t.Fatal("purged directory kept its basis")
	}
}

// A listing read after the filers named positions holds everything up to
// them, and releases waiters for them.
func TestNoteDirectoryBasisReleasesWaiters(t *testing.T) {
	mc, _, _, _ := newTestMetaCache(t, map[util.FullPath]bool{"/": true, "/dir": true})
	defer mc.Shutdown()
	mc.NoteDirectoryBasis("/dir", nil)

	want := []*filer_pb.FilerDirectoryPosition{at(filerA, 500, true), at(filerB, 70, true)}
	released := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		released <- mc.AwaitDirectoryPositions(ctx, "/dir", want)
	}()
	mc.NoteDirectoryBasis("/dir", want)
	if !<-released {
		t.Fatal("waiter not released")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if mc.AwaitDirectoryPositions(ctx, "/dir", []*filer_pb.FilerDirectoryPosition{at(filerB, 71, true)}) {
		t.Fatal("waiter for an undelivered change released")
	}
}
