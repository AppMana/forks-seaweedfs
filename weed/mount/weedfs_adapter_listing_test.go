package mount

import (
	"context"
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
