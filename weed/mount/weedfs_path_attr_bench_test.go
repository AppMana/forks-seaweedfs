package mount

import (
	"runtime"
	"testing"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func TestCachedMetadataOpsDoNotSpawnCancelGoroutines(t *testing.T) {
	cancel := make(chan struct{})
	baseGoroutines := runtime.NumGoroutine()
	ctx, cancelFn := ctxFromCancel(cancel)
	defer cancelFn()
	if got := runtime.NumGoroutine(); got > baseGoroutines {
		t.Fatalf("ctxFromCancel spawned a goroutine on creation: goroutines=%d, base=%d", got, baseGoroutines)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("ctx.Err() = %v, want nil", err)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		c, fn := ctxFromCancel(cancel)
		_ = c.Done()
		fn()
	}); allocs > 0 {
		t.Fatalf("ctxFromCancel allocs = %v, want 0", allocs)
	}

	wfs := newPagingWFS(t, "/dir", []string{"file"}, 0)
	dirInode, found := wfs.inodeToPath.GetInode("/dir")
	if !found {
		t.Fatal("missing /dir inode")
	}
	fileInode := wfs.inodeToPath.Lookup("/dir/file", 1, false, false, 42, true)
	wfs.fhMap.AcquireFileHandle(wfs, fileInode, &filer_pb.Entry{
		Name: "file", Attributes: &filer_pb.FuseAttributes{FileSize: 1, FileMode: 0644},
	}, 0, 0)

	var attrOut fuse.AttrOut
	attrIn := fuse.GetAttrIn{InHeader: fuse.InHeader{NodeId: fileInode}}
	openAllocs := testing.AllocsPerRun(100, func() {
		if code := wfs.GetAttr(cancel, &attrIn, &attrOut); code != fuse.OK {
			t.Fatalf("GetAttr = %v", code)
		}
	})
	if openAllocs > 4 {
		t.Fatalf("open-handle GetAttr allocs = %v, want <= 4 (no cancelCtx/goroutine)", openAllocs)
	}

	var entryOut fuse.EntryOut
	lookupHeader := fuse.InHeader{NodeId: dirInode}
	lookupWithCancelAllocs := testing.AllocsPerRun(100, func() {
		if code := wfs.Lookup(cancel, &lookupHeader, "file", &entryOut); code != fuse.OK {
			t.Fatalf("Lookup = %v", code)
		}
	})
	lookupNilCancelAllocs := testing.AllocsPerRun(100, func() {
		if code := wfs.Lookup(nil, &lookupHeader, "file", &entryOut); code != fuse.OK {
			t.Fatalf("Lookup = %v", code)
		}
	})
	if lookupWithCancelAllocs != lookupNilCancelAllocs {
		t.Fatalf("cached Lookup with cancel allocated %v vs %v without cancel", lookupWithCancelAllocs, lookupNilCancelAllocs)
	}
}

// BenchmarkPathGetAttr measures the shared path-to-inode/attribute hot path
// used by the Windows adapter, with and without a surviving open handle.
// Setup is outside the timer; no filer or VM is required.
func BenchmarkPathGetAttr(b *testing.B) {
	cancel := make(chan struct{})
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
				if code := wfs.GetAttr(cancel, &in, &out); code != fuse.OK || out.Size != 1 {
					b.Fatalf("GetAttr = %v size=%d", code, out.Size)
				}
			}
		})
	}
}
