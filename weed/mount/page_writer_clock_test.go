package mount

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/mount/page_writer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

type timestampRecordingPages struct {
	page_writer.DirtyPages
	timestamps []int64
}

func (r *timestampRecordingPages) AddPage(offset int64, data []byte, sequential bool, ts int64) error {
	r.timestamps = append(r.timestamps, ts)
	return r.DirtyPages.AddPage(offset, data, sequential, ts)
}

// A sequential overwrite must outrank the version this client already read,
// even when the previous writer's clock is ahead. Exercise the actual Write
// path and the actual compaction winner selection, without an upload server.
func TestWriteAfterFutureChunkSurvivesCompaction(t *testing.T) {
	wfs := newInvalidateTestWFS(t)
	inode := wfs.inodeToPath.Lookup(util.FullPath("/file"), time.Now().Unix(), false, false, 0, false)
	old := &filer_pb.FileChunk{FileId: "1,0100000001", Size: 4, ModifiedTsNs: time.Now().Add(time.Hour).UnixNano()}
	fh, _ := wfs.fhMap.AcquireFileHandle(wfs, inode, &filer_pb.Entry{Name: "file", Attributes: &filer_pb.FuseAttributes{FileSize: 4}, Chunks: []*filer_pb.FileChunk{old}}, 0, 0)
	r := &timestampRecordingPages{DirtyPages: fh.dirtyPages.randomWriter}
	fh.dirtyPages.randomWriter = r
	t.Cleanup(fh.dirtyPages.Destroy)
	for i := 0; i < 2; i++ {
		written, status := wfs.Write(nil, &fuse.WriteIn{Fh: uint64(fh.fh), Size: 4}, []byte("new!"))
		if written != 4 || status != fuse.OK {
			t.Fatalf("write: %d, %v", written, status)
		}
		current := &filer_pb.FileChunk{FileId: fmt.Sprintf("1,%02x00000001", i+2), Size: 4, ModifiedTsNs: r.timestamps[i]}
		live, garbage := filer.CompactFileChunks(context.Background(), nil, []*filer_pb.FileChunk{old, current})
		if len(live) != 1 || live[0] != current || len(garbage) != 1 || garbage[0] != old {
			t.Fatalf("acknowledged overwrite discarded: old timestamp=%d write timestamp=%d live=%v", old.ModifiedTsNs, current.ModifiedTsNs, live)
		}
		old = current
	}
	if got := fh.GetEntry().Attributes.Mtime; got > time.Now().Add(time.Minute).Unix() {
		t.Fatalf("logical chunk timestamp leaked into POSIX mtime: %d", got)
	}
}

func TestPageWriterRejectsTimestampExhaustion(t *testing.T) {
	group, err := filer.NewChunkGroup(nil, nil, []*filer_pb.FileChunk{{Size: 4, ModifiedTsNs: math.MaxInt64}}, 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	// No backing writer: touching it instead of rejecting would panic.
	pw := &PageWriter{fh: &FileHandle{entryChunkGroup: group}, chunkSize: 1024}
	if err := pw.AddPage(0, []byte("new!"), false, 1); err == nil {
		t.Fatal("acknowledged a write whose timestamp would overflow")
	}
}
