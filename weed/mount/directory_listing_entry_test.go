package mount

import (
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func TestDirectoryListingUsesLocalHandleMetadata(t *testing.T) {
	wfs := &WFS{fhMap: NewFileHandleToInode(), fhLockTable: util.NewLockTable[FileHandleId]()}
	stale := &filer.Entry{FullPath: "/dir/asset", Attr: filer.Attr{FileSize: 7}}
	if got := wfs.directoryListingEntry("/dir", 42, stale); got != stale {
		t.Fatal("unopened entries should retain the listing fast path")
	}
	entry := &filer_pb.Entry{Name: "asset", Attributes: &filer_pb.FuseAttributes{FileSize: 14, Mtime: 9, FileMode: 0600}}
	entry.Extended = map[string][]byte{"test": []byte("before")}
	fh := &FileHandle{fh: 1, inode: 42, entry: &LockedEntry{Entry: entry}}
	wfs.fhMap.inode2fh[42] = fh
	got := wfs.directoryListingEntry("/dir", 42, stale)
	if got.FileSize != 14 || got.Mtime.Unix() != 9 || got.Mode != 0600 {
		t.Fatalf("directory listing retained stale metadata: %+v", got.Attr)
	}
	if stale.FileSize != 7 {
		t.Fatal("listing cache entry was mutated")
	}
	entry.Extended["test"][0] = 'X'
	if string(got.Extended["test"]) != "before" {
		t.Fatal("directory snapshot shares mutable handle metadata")
	}
}
