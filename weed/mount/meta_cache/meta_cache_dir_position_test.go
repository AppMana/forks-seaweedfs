package meta_cache

import (
	"context"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

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

// A cached listing is current through every change applied to its children,
// and a waiter for a change still on its way is released by its arrival.
func TestDirectoryCurrentThroughFollowsAppliedChanges(t *testing.T) {
	mc, _, _, _ := newTestMetaCache(t, map[util.FullPath]bool{"/": true, "/dir": true})
	defer mc.Shutdown()

	if got := mc.DirectoryCurrentThrough("/dir"); got != 0 {
		t.Fatalf("fresh directory current through %d, want 0", got)
	}

	released := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		released <- mc.AwaitDirectoryCurrentThrough(ctx, "/dir", 200)
	}()

	if err := mc.ApplyMetadataResponse(context.Background(), createEventAt("/dir", "a", 100), SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	if got := mc.DirectoryCurrentThrough("/dir"); got != 100 {
		t.Fatalf("current through %d after a change at 100", got)
	}
	select {
	case <-released:
		t.Fatal("waiter for 200 released by a change at 100")
	case <-time.After(50 * time.Millisecond):
	}

	if err := mc.ApplyMetadataResponse(context.Background(), createEventAt("/dir", "b", 200), SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	if !<-released {
		t.Fatal("waiter for 200 not released by the change at 200")
	}
	if entry, _, err := mc.FindEntry(context.Background(), "/dir/b"); err != nil || entry == nil {
		t.Fatalf("listing current through 200 does not hold the change at 200: %v", err)
	}
}

// A rename across directories changes both parents, and a moved directory's
// own children.
func TestDirectoryCurrentThroughCoversRenameParentsAndMovedDirectory(t *testing.T) {
	mc, _, _, _ := newTestMetaCache(t, map[util.FullPath]bool{"/": true, "/a": true, "/b": true, "/b/sub": true})
	defer mc.Shutdown()

	rename := &filer_pb.SubscribeMetadataResponse{
		Directory: "/a",
		TsNs:      300,
		EventNotification: &filer_pb.EventNotification{
			OldEntry:      &filer_pb.Entry{Name: "sub", IsDirectory: true},
			NewEntry:      &filer_pb.Entry{Name: "sub", IsDirectory: true, Attributes: &filer_pb.FuseAttributes{FileMode: 040755}},
			NewParentPath: "/b",
		},
	}
	if err := mc.ApplyMetadataResponse(context.Background(), rename, SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []util.FullPath{"/a", "/b", "/b/sub"} {
		if got := mc.DirectoryCurrentThrough(dir); got != 300 {
			t.Errorf("%s current through %d, want 300", dir, got)
		}
	}
}

// An uncached directory reads through to the filer and keeps no position.
func TestDirectoryCurrentThroughSkipsUncachedDirectories(t *testing.T) {
	mc, _, _, _ := newTestMetaCache(t, map[util.FullPath]bool{"/": true})
	defer mc.Shutdown()

	if err := mc.ApplyMetadataResponse(context.Background(), createEventAt("/elsewhere", "x", 100), SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	mc.RLock()
	_, found := mc.dirChangesApplied["/elsewhere"]
	mc.RUnlock()
	if found {
		t.Fatal("position recorded for an uncached directory")
	}
}

// Dropping a directory's cached listing drops its position with it: a rebuilt
// listing starts from its own snapshot.
func TestDirectoryCurrentThroughResetByPurge(t *testing.T) {
	cached := map[util.FullPath]bool{"/": true, "/dir": true}
	mc, _, _, _ := newTestMetaCache(t, cached)
	defer mc.Shutdown()

	if err := mc.ApplyMetadataResponse(context.Background(), createEventAt("/dir", "a", 100), SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	mc.PurgeDirectoryChildren("/dir", func() { close(done) })
	<-done
	// The purge runs on the apply loop; a later request is ordered after it.
	if err := mc.ApplyMetadataResponse(context.Background(), createEventAt("/other", "x", 1), SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	if got := mc.DirectoryCurrentThrough("/dir"); got != 0 {
		t.Fatalf("purged directory current through %d, want 0", got)
	}
}

// A listing read after the filers named a change covers it, even one with no
// snapshot of its own.
func TestNoteDirectoryCurrentThroughReleasesWaiters(t *testing.T) {
	mc, _, _, _ := newTestMetaCache(t, map[util.FullPath]bool{"/": true, "/dir": true})
	defer mc.Shutdown()

	released := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		released <- mc.AwaitDirectoryCurrentThrough(ctx, "/dir", 500)
	}()
	mc.NoteDirectoryCurrentThrough("/dir", 500)
	if !<-released {
		t.Fatal("waiter not released")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if mc.AwaitDirectoryCurrentThrough(ctx, "/dir", 501) {
		t.Fatal("waiter for an undelivered change released")
	}
}
