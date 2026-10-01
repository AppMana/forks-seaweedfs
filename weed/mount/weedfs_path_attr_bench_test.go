package mount

import (
	"testing"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// BenchmarkPathGetAttr measures the shared path-to-inode/attribute hot path
// used by the Windows adapter, with and without a surviving open handle.
// Setup is outside the timer; no filer or VM is required.
func BenchmarkPathGetAttr(b *testing.B) {
	for _, opened := range []bool{false, true} {
		name := "cached"
		if opened {
			name = "open"
		}
		b.Run(name, func(b *testing.B) {
			wfs := newPagingWFS(b, "/dir", []string{"file"}, 0)
			path := util.FullPath("/dir/file")
			inode := wfs.inodeToPath.Lookup(path, 1, false, false, 42, true)
			if opened {
				wfs.fhMap.AcquireFileHandle(wfs, inode, &filer_pb.Entry{
					Name: "file", Attributes: &filer_pb.FuseAttributes{FileSize: 1, FileMode: 0644},
				}, 0, 0)
			}
			var out fuse.AttrOut
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ino, found := wfs.inodeToPath.GetInode(path)
				if !found {
					b.Fatal("live name disappeared")
				}
				in := fuse.GetAttrIn{InHeader: fuse.InHeader{NodeId: ino}}
				if code := wfs.GetAttr(nil, &in, &out); code != fuse.OK || out.Size != 1 {
					b.Fatalf("GetAttr = %v size=%d", code, out.Size)
				}
			}
		})
	}
}
