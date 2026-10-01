package mount

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func TestAdapterListingReadsThroughOversizedDirectory(t *testing.T) {
	wfs := newLookupCacheTestWFS(t, 60)
	wfs.option.CacheDirMaxEntries = 1
	fake := &lookupCacheTestFiler{
		entries: []*filer_pb.Entry{
			{Name: "a", Attributes: &filer_pb.FuseAttributes{FileMode: 0100644}},
			{Name: "b", Attributes: &filer_pb.FuseAttributes{FileMode: 0100644}},
			{Name: "c", Attributes: &filer_pb.FuseAttributes{FileMode: 0100644}},
		},
		snapshotTsNs: 5000,
	}
	startFakeFiler(t, wfs, fake)
	for _, stopAfterFirst := range []bool{false, true} {
		var names []string
		err := wfs.listDirectoryForAdapter(context.Background(), util.FullPath("/"), func(entry *filer.Entry) (bool, error) {
			names = append(names, entry.Name())
			return !stopAfterFirst, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"a", "b", "c"}
		if stopAfterFirst {
			want = want[:1]
		}
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("listing = %v, want %v", names, want)
		}
		if wfs.metaCache.IsDirectoryCached("/") {
			t.Fatal("oversized directory must not become an authoritative partial cache")
		}
	}
}

func TestAdapterListingCallbackDoesNotHoldMetadataLock(t *testing.T) {
	wfs := newLookupCacheTestWFS(t, 60)
	startFakeFiler(t, wfs, &lookupCacheTestFiler{
		entries:      []*filer_pb.Entry{{Name: "a", Attributes: &filer_pb.FuseAttributes{FileMode: 0100644}}},
		snapshotTsNs: 5000,
	})
	called := false
	err := wfs.listDirectoryForAdapter(context.Background(), "/", func(entry *filer.Entry) (bool, error) {
		called = true
		// A handle snapshot may wait for Flush, which needs this lock.
		// Detect the inversion without leaving a deadlocked test goroutine.
		if !wfs.metaCache.TryLock() {
			t.Error("adapter callback holds metadata lock while it may acquire a file handle lock")
		} else {
			wfs.metaCache.Unlock()
		}
		return true, nil
	})
	if err != nil || !called {
		t.Fatalf("callback called=%v error=%v", called, err)
	}
}

func TestAdapterListingCachedPageBoundaries(t *testing.T) {
	wfs := newLookupCacheTestWFS(t, 60)
	fake := &lookupCacheTestFiler{snapshotTsNs: 5000}
	for i := 0; i < 257; i++ {
		fake.entries = append(fake.entries, &filer_pb.Entry{Name: fmt.Sprintf("file-%03d", i), Attributes: &filer_pb.FuseAttributes{FileMode: 0100644}})
	}
	startFakeFiler(t, wfs, fake)
	for _, limit := range []int{1, 128, 129, 257} {
		seen := 0
		err := wfs.listDirectoryForAdapter(context.Background(), "/", func(entry *filer.Entry) (bool, error) {
			if want := fmt.Sprintf("file-%03d", seen); entry.Name() != want {
				t.Fatalf("entry %d = %q, want %q", seen, entry.Name(), want)
			}
			seen++
			return seen < limit, nil
		})
		if err != nil || seen != limit {
			t.Fatalf("limit=%d seen=%d error=%v", limit, seen, err)
		}
	}
}
