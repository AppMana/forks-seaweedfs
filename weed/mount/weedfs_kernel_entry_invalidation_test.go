package mount

import (
	"context"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

type entryNotification struct {
	parent uint64
	name   string
}

func TestForeignRenameExpiresBothKernelNames(t *testing.T) {
	wfs := newInvalidateTestWFS(t)
	now := time.Now().Unix()
	parent := wfs.inodeToPath.Lookup(util.FullPath("/dir"), now, true, false, 0, true)
	inode := wfs.inodeToPath.Lookup(util.FullPath("/dir/old"), now, false, false, 0, true)
	notifier := &recordingInodeNotifier{}
	wfs.fuseServer = notifier
	event := &filer_pb.SubscribeMetadataResponse{Directory: "/dir", TsNs: 1000,
		EventNotification: &filer_pb.EventNotification{
			OldEntry: &filer_pb.Entry{Name: "old"}, NewEntry: &filer_pb.Entry{Name: "new"},
			Signatures: []int32{wfs.signature + 1},
		}}
	if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), event, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	wfs.metaCache.WaitForEntryInvalidations()
	if path, status := wfs.inodeToPath.GetPath(inode); status != fuse.OK || path != "/dir/new" {
		t.Fatalf("rename did not update userspace path: %s, %v", path, status)
	}
	// Expiring the directory inode's data does not expire positive old-name
	// dentries or negative new-name dentries: require the actual kernel API.
	for _, name := range []string{"old", "new"} {
		found := false
		for _, call := range notifier.entryCalls {
			if call.parent == parent && call.name == name {
				found = true
			}
		}
		if !found {
			t.Errorf("rename left kernel name %q cached; notifications=%v", name, notifier.entryCalls)
		}
	}
}

func TestKernelEntryNotificationOnlyForForeignNamespaceChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event meta_cache.EntryInvalidation
		want  bool
	}{
		{"create", meta_cache.EntryInvalidation{Entry: &filer_pb.Entry{Name: "file"}}, true},
		{"delete", meta_cache.EntryInvalidation{Deleted: true}, true},
		{"content-update", meta_cache.EntryInvalidation{PreviousEntry: &filer_pb.Entry{Name: "file"}, Entry: &filer_pb.Entry{Name: "file"}}, false},
		{"self-create", meta_cache.EntryInvalidation{Entry: &filer_pb.Entry{Name: "file"}, Signatures: []int32{1}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wfs := newInvalidateTestWFS(t)
			wfs.inodeToPath.Lookup(util.FullPath("/dir"), time.Now().Unix(), true, false, 0, true)
			notifier := &recordingInodeNotifier{}
			wfs.fuseServer = notifier
			tc.event.Path = "/dir/file"
			wfs.onEntryInvalidation(tc.event)
			if got := len(notifier.entryCalls) > 0; got != tc.want {
				t.Fatalf("entry invalidated=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestCrossDirectoryRenameNotifiesAfterOpenHandleMoves(t *testing.T) {
	wfs := newInvalidateTestWFS(t)
	now := time.Now().Unix()
	sourceParent := wfs.inodeToPath.Lookup("/source", now, true, false, 0, true)
	destParent := wfs.inodeToPath.Lookup("/dest", now, true, false, 0, true)
	inode := wfs.inodeToPath.Lookup("/source/old", now, false, false, 0, true)
	fh, _ := wfs.fhMap.AcquireFileHandle(wfs, inode, &filer_pb.Entry{Name: "old", Content: []byte("intact"), Attributes: &filer_pb.FuseAttributes{FileSize: 6}}, 0, 0)
	t.Cleanup(fh.dirtyPages.Destroy)
	notifier := &recordingInodeNotifier{}
	wfs.fuseServer = notifier
	notifier.onEntryNotify = func(uint64, string) {
		// Kernel re-entry must see the moved handle, without deadlocking on
		// the lock that the metadata worker used to update that handle.
		done := make(chan struct{})
		go func() {
			lock := wfs.fhLockTable.AcquireLock("test-entry-notify", fh.fh, util.ExclusiveLock)
			defer wfs.fhLockTable.ReleaseLock(fh.fh, lock)
			if fh.FullPath() != "/dest/new" || fh.isDeleted || string(fh.GetEntry().Content) != "intact" {
				t.Errorf("notification observed unmoved/deleted/damaged handle: path=%s deleted=%v content=%q", fh.FullPath(), fh.isDeleted, fh.GetEntry().Content)
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("entry notification ran while handle lock held")
		}
	}
	event := &filer_pb.SubscribeMetadataResponse{Directory: "/source", TsNs: 1000, EventNotification: &filer_pb.EventNotification{
		OldEntry: &filer_pb.Entry{Name: "old"}, NewEntry: &filer_pb.Entry{Name: "new", Content: []byte("intact"), Attributes: &filer_pb.FuseAttributes{FileSize: 6}}, NewParentPath: "/dest", Signatures: []int32{wfs.signature + 1},
	}}
	if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), event, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	wfs.metaCache.WaitForEntryInvalidations()
	want := []entryNotification{{sourceParent, "old"}, {destParent, "new"}}
	if len(notifier.entryCalls) != len(want) {
		t.Fatalf("notifications=%v want=%v", notifier.entryCalls, want)
	}
	for i := range want {
		if notifier.entryCalls[i] != want[i] {
			t.Fatalf("notifications=%v want=%v", notifier.entryCalls, want)
		}
	}
}
