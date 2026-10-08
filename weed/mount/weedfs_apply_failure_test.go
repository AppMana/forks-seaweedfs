package mount

import (
	"errors"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// After the local store fails to record a subscribed change, no directory may
// keep answering lookups or listings from the cache that missed it.
func TestMetadataApplyFailureReadsEveryDirectoryThroughTheFiler(t *testing.T) {
	wfs := newInvalidateTestWFS(t)
	now := time.Now().Unix()
	for _, dir := range []util.FullPath{"/dir", "/other"} {
		wfs.inodeToPath.Lookup(dir, now, true, false, 0, true)
		wfs.inodeToPath.MarkChildrenCached(dir)
		if !wfs.inodeToPath.IsChildrenCached(dir) {
			t.Fatalf("%s not cached before the failure", dir)
		}
	}
	wfs.onMetadataApplyFailure(&filer_pb.SubscribeMetadataResponse{Directory: "/dir"}, errors.New("no space left on device"))
	for _, dir := range []util.FullPath{"/dir", "/other"} {
		if wfs.inodeToPath.IsChildrenCached(dir) {
			t.Errorf("%s still served from the cache after a store failure", dir)
		}
	}
}
