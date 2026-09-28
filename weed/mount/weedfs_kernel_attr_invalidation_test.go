package mount

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func TestKernelAttributeNotificationPreservesDirtyWrites(t *testing.T) {
	wfs := newInvalidateTestWFS(t)
	inode := wfs.inodeToPath.Lookup(util.FullPath("/dir/file"), time.Now().Unix(), false, false, 0, false)
	fh, _ := wfs.fhMap.AcquireFileHandle(wfs, inode, &filer_pb.Entry{Name: "file", Attributes: &filer_pb.FuseAttributes{FileSize: 100}}, 0, 0)
	want := []byte("local-dirty")
	if err := fh.dirtyPages.AddPage(100, want, false, 2000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fh.dirtyPages.Destroy)
	fh.UpdateEntry(func(entry *filer_pb.Entry) { entry.Attributes.FileSize = 100 + uint64(len(want)) })
	fh.dirtyMetadata = true
	notifier := &recordingInodeNotifier{}
	wfs.fuseServer = notifier
	// Foreign chmod changes metadata, not the committed base content.
	event := updateEventFor("file", 100, 1000)
	// updateEventFor otherwise describes a size change from zero. A chmod
	// fixture must carry the actual unchanged committed size on both sides.
	event.EventNotification.OldEntry.Attributes = &filer_pb.FuseAttributes{FileSize: 100}
	event.EventNotification.NewEntry.Attributes.FileMode = 0600
	event.EventNotification.Signatures = []int32{wfs.signature + 1}
	if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), event, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	wfs.metaCache.WaitForEntryInvalidations()
	got := make([]byte, len(want))
	stop := fh.dirtyPages.ReadDirtyDataAt(got, 100, 0)
	if stop != int64(100+len(want)) || !bytes.Equal(got, want) || fh.GetEntry().Attributes.FileSize != uint64(100+len(want)) {
		t.Fatalf("notification lost local dirty bytes: stop=%d data=%q size=%d", stop, got, fh.GetEntry().Attributes.FileSize)
	}
	if len(notifier.calls) != 1 || notifier.calls[0].inode != inode || notifier.calls[0].offset >= 0 {
		t.Fatalf("expected attribute-only notification: %+v", notifier.calls)
	}
}

type inodeNotification struct {
	inode          uint64
	offset, length int64
}

func TestForeignSameMtimeContentChangeInvalidatesKernelData(t *testing.T) {
	wfs := newInvalidateTestWFS(t)
	inode := wfs.inodeToPath.Lookup(util.FullPath("/dir/file"), time.Now().Unix(), false, false, 0, false)
	notifier := &recordingInodeNotifier{}
	wfs.fuseServer = notifier
	event := updateEventFor("file", 4, 1000)
	event.EventNotification.OldEntry = &filer_pb.Entry{Name: "file", Content: []byte("old!"), Attributes: &filer_pb.FuseAttributes{FileSize: 4}}
	event.EventNotification.NewEntry.Content = []byte("new!")
	event.EventNotification.Signatures = []int32{wfs.signature + 1}
	if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), event, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	wfs.metaCache.WaitForEntryInvalidations()
	for _, call := range notifier.calls {
		if call.inode == inode && call.offset == 0 && call.length <= 0 {
			return
		}
	}
	t.Fatalf("same-mtime content update did not invalidate cached data: %+v", notifier.calls)
}

func TestEntryListenerSeesRefreshedOpenHandle(t *testing.T) {
	wfs := newInvalidateTestWFS(t)
	inode := wfs.inodeToPath.Lookup(util.FullPath("/dir/file"), time.Now().Unix(), false, false, 0, false)
	fh, _ := wfs.fhMap.AcquireFileHandle(wfs, inode, &filer_pb.Entry{Name: "file", Attributes: &filer_pb.FuseAttributes{}}, 0, 0)
	called := false
	wfs.SetEntryChangeListener(func(meta_cache.EntryInvalidation) {
		called = true
		if got := fh.GetEntry().Attributes.FileSize; got != 113 {
			t.Errorf("listener observed stale open handle: size=%d", got)
		}
	})
	event := updateEventFor("file", 113, 1000)
	event.EventNotification.Signatures = []int32{wfs.signature + 1}
	if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), event, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	wfs.metaCache.WaitForEntryInvalidations()
	if !called {
		t.Fatal("listener not called")
	}
}

type recordingInodeNotifier struct {
	entryCalls    []entryNotification
	onEntryNotify func(uint64, string)
	calls         []inodeNotification
	onNotify      func()
}

func (n *recordingInodeNotifier) EntryNotify(parent uint64, name string) fuse.Status {
	n.entryCalls = append(n.entryCalls, entryNotification{parent, name})
	if n.onEntryNotify != nil {
		n.onEntryNotify(parent, name)
	}
	return fuse.OK
}

func (n *recordingInodeNotifier) InodeNotify(inode uint64, offset, length int64) fuse.Status {
	n.calls = append(n.calls, inodeNotification{inode, offset, length})
	if n.onNotify != nil {
		n.onNotify()
	}
	return fuse.OK
}

// A successful 113-byte FUSE READ can still appear as EOF to the process if
// Linux retains the inode's old zero length. Updating only the directory or
// the mount's metadata store cannot expire that kernel attribute cache.
func TestForeignUpdateInvalidatesKernelFileAttributes(t *testing.T) {
	for _, open := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "open"}[open], func(t *testing.T) {
			wfs := newInvalidateTestWFS(t)
			inode := wfs.inodeToPath.Lookup(util.FullPath("/dir/file"), time.Now().Unix(), false, false, 0, false)
			if open {
				wfs.fhMap.AcquireFileHandle(wfs, inode, &filer_pb.Entry{Name: "file", Attributes: &filer_pb.FuseAttributes{}}, 0, 0)
			}
			notifier := &recordingInodeNotifier{}
			if open {
				notifier.onNotify = func() {
					// Model a kernel attribute revalidation during notification.
					// It must see the refreshed handle without a held handle lock.
					done := make(chan fuse.AttrOut, 1)
					go func() {
						var out fuse.AttrOut
						if status := wfs.GetAttr(nil, &fuse.GetAttrIn{InHeader: fuse.InHeader{NodeId: inode}}, &out); status != fuse.OK {
							t.Errorf("GetAttr: %v", status)
						}
						done <- out
					}()
					select {
					case out := <-done:
						if out.Size != 113 {
							t.Errorf("notification preceded handle refresh: size=%d", out.Size)
						}
					case <-time.After(time.Second):
						t.Error("notification holds a lock needed by GetAttr")
					}
				}
			}
			wfs.fuseServer = notifier
			event := updateEventFor("file", 113, 1000)
			event.EventNotification.Signatures = []int32{wfs.signature + 1}
			if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), event, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
				t.Fatal(err)
			}
			wfs.metaCache.WaitForEntryInvalidations()
			found := false
			for _, call := range notifier.calls {
				if call.inode == inode {
					found = true
					// This event grows content from zero to 113 bytes. Invalidate
					// data as well as attributes through the kernel's cache API.
					if call.offset != 0 || call.length != 0 {
						t.Fatalf("content update did not invalidate file cache: %+v", call)
					}
				}
			}
			if !found {
				t.Fatalf("foreign update did not expire file inode %d: %+v", inode, notifier.calls)
			}
		})
	}
}

func TestSelfUpdateDoesNotInvalidateKernelFile(t *testing.T) {
	wfs := newInvalidateTestWFS(t)
	inode := wfs.inodeToPath.Lookup(util.FullPath("/dir/file"), time.Now().Unix(), false, false, 0, false)
	notifier := &recordingInodeNotifier{}
	wfs.fuseServer = notifier
	event := updateEventFor("file", 113, 1000)
	event.EventNotification.Signatures = []int32{wfs.signature}
	if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), event, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	wfs.metaCache.WaitForEntryInvalidations()
	for _, call := range notifier.calls {
		if call.inode == inode {
			t.Fatalf("self echo invalidated file: %+v", call)
		}
	}
}
