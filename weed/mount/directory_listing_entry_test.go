package mount

import (
	"fmt"
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

func TestDirectoryListingUnopenedEntryDoesNotAllocate(t *testing.T) {
	wfs := &WFS{fhMap: NewFileHandleToInode(), fhLockTable: util.NewLockTable[FileHandleId]()}
	entry := &filer.Entry{FullPath: "/dir/asset", Attr: filer.Attr{FileSize: 7}}
	if allocations := testing.AllocsPerRun(1000, func() {
		if wfs.directoryListingEntry("/dir", 42, entry) != entry {
			panic("unopened directory entry was replaced")
		}
	}); allocations != 0 {
		t.Fatalf("ordinary unopened entry incurred %g allocations", allocations)
	}
}

func TestDirectoryListingSnapshotAllocationsDoNotScaleWithChunks(t *testing.T) {
	wfs := &WFS{fhMap: NewFileHandleToInode(), fhLockTable: util.NewLockTable[FileHandleId]()}
	pb := &filer_pb.Entry{Name: "asset", Attributes: &filer_pb.FuseAttributes{FileSize: 7}, HardLinkCounter: 3}
	wfs.fhMap.inode2fh[42] = &FileHandle{fh: 1, inode: 42, entry: &LockedEntry{Entry: pb}}
	measure := func() float64 {
		return testing.AllocsPerRun(50, func() { _ = wfs.directoryListingEntry("/dir", 42, nil) })
	}
	empty := measure()
	for i := 0; i < 4096; i++ {
		pb.Chunks = append(pb.Chunks, &filer_pb.FileChunk{Offset: int64(i) * 1024, Size: 1024})
	}
	full := measure()
	if full > empty {
		t.Errorf("enumeration copied chunk records: empty=%g allocations, 4096 chunks=%g", empty, full)
	}
	snapshot := wfs.directoryListingEntry("/dir", 42, nil)
	if snapshot.FileSize != 4096*1024 || snapshot.HardLinkCounter != 3 {
		t.Fatalf("metadata snapshot lost size or link count: %+v", snapshot)
	}
	pb.Chunks[4095].Size = 2048
	if snapshot.FileSize != 4096*1024 {
		t.Fatal("snapshot changed after source mutation")
	}
	pb.RemoteEntry = &filer_pb.RemoteEntry{RemoteMtime: 1, RemoteSize: 8192 * 1024}
	if got := wfs.directoryListingEntry("/dir", 42, nil); got.FileSize != 8192*1024 {
		t.Fatalf("remote size lost: %d", got.FileSize)
	}
}

// Measure the unchanged common path separately from snapshotting open handles.
// Chunk-heavy files make the snapshot cost visible instead of hiding it in an
// open/read/close benchmark that never enumerates a directory.
func BenchmarkDirectoryListingMetadata(b *testing.B) {
	for _, chunks := range []int{-1, 0, 1, 128, 4096} {
		name := "unopened"
		if chunks >= 0 {
			name = fmt.Sprintf("open/chunks=%d", chunks)
		}
		b.Run(name, func(b *testing.B) {
			wfs := &WFS{fhMap: NewFileHandleToInode(), fhLockTable: util.NewLockTable[FileHandleId]()}
			entry := &filer.Entry{FullPath: "/dir/asset", Attr: filer.Attr{FileSize: 7}}
			if chunks >= 0 {
				pb := &filer_pb.Entry{Name: "asset", Attributes: &filer_pb.FuseAttributes{FileSize: 14}}
				for i := 0; i < chunks; i++ {
					pb.Chunks = append(pb.Chunks, &filer_pb.FileChunk{FileId: "1,01234567", Offset: int64(i) * 1024, Size: 1024})
				}
				wfs.fhMap.inode2fh[42] = &FileHandle{fh: 1, inode: 42, entry: &LockedEntry{Entry: pb}}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if wfs.directoryListingEntry("/dir", 42, entry) == nil {
					b.Fatal("missing entry")
				}
			}
		})
	}
}
