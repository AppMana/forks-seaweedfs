package mount

import (
	"context"
	"os"
	"testing"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/protobuf/proto"
)

type noChangeAckFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
}

func (*noChangeAckFiler) UpdateEntry(context.Context, *filer_pb.UpdateEntryRequest) (*filer_pb.UpdateEntryResponse, error) {
	return &filer_pb.UpdateEntryResponse{LogTsNs: 100, LogSignature: 7}, nil
}

// Exercise the real no-event RPC acknowledgment fallback in saveEntry, then
// the same cached directory listing used to resolve .GIT on Windows. An inode
// mapping alone cannot repair an omitted entry in that case-fold scan.
func TestSaveEntryNoChangeAckPreservesCaseFoldListing(t *testing.T) {
	wfs := newLookupCacheTestWFS(t, 60)
	wfs.option.FilerMountRootPath = "/"
	startFakeFiler(t, wfs, &noChangeAckFiler{})
	wfs.inodeToPath.MarkChildrenCached("/")
	wfs.inodeToPath.Lookup("/.git", 1, true, false, 42, true)
	entry := &filer_pb.Entry{Name: ".git", IsDirectory: true, Attributes: &filer_pb.FuseAttributes{FileMode: uint32(os.ModeDir) | 0755, Inode: 42}}
	event := metadataCreateEvent("/", entry)
	event.TsNs = 100
	if err := wfs.applyLocalMetadataEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if code := wfs.saveEntry("/.git", proto.Clone(entry).(*filer_pb.Entry)); code != fuse.OK {
		t.Fatalf("saveEntry: %v", code)
	}
	found := false
	if err := wfs.listDirectoryForAdapter(context.Background(), "/", func(e *filer.Entry) (bool, error) {
		if winFspNameEqual(".GIT", e.Name(), false) {
			found = true
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("successful no-change save made .GIT unresolvable in the Windows case-fold listing")
	}
}
