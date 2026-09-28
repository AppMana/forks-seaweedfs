package mount

import (
	"bytes"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/protobuf/proto"
)

// Cover accepted partial chunks, including one request spanning two chunks.
// Neither chunk is complete, so these checks need no uploader or volume server.
func TestAcceptedWriteUpdatesEntryAndDirtyBytes(t *testing.T) {
	for _, offset := range []uint64{0, 1020} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			wfs, _ := newCreateTestWFS(t)
			out := &fuse.CreateOut{}
			if status := wfs.Create(nil, &fuse.CreateIn{
				InHeader: fuse.InHeader{NodeId: 1}, Flags: syscall.O_RDWR | syscall.O_CREAT, Mode: 0o644,
			}, "accepted", out); status != fuse.OK {
				t.Fatal(status)
			}
			fh := wfs.GetHandle(FileHandleId(out.Fh))
			t.Cleanup(fh.dirtyPages.Destroy)
			wfs.option.Quota = 1 << 30
			uncommittedBefore := wfs.GetUncommittedBytes()
			t.Cleanup(func() { wfs.SubtractUncommittedBytes(wfs.GetUncommittedBytes() - uncommittedBefore) })
			data := []byte("accepted")
			n, status := wfs.Write(nil, &fuse.WriteIn{InHeader: fuse.InHeader{NodeId: out.NodeId}, Fh: out.Fh, Offset: offset, Size: uint32(len(data))}, data)
			if n != uint32(len(data)) || status != fuse.OK {
				t.Fatalf("Write = (%d, %v)", n, status)
			}
			if size := fh.GetEntry().Attributes.FileSize; size != offset+uint64(len(data)) {
				t.Fatalf("size = %d", size)
			}
			if !fh.dirtyMetadata || fh.GetEntry().Attributes.Mtime == 0 {
				t.Fatal("accepted write did not update metadata")
			}
			if got := wfs.GetUncommittedBytes() - uncommittedBefore; got != int64(offset)+int64(len(data)) {
				t.Fatalf("quota charged = %d", got)
			}
			got := make([]byte, len(data))
			fh.dirtyPages.ReadDirtyDataAt(got, int64(offset), time.Now().Add(time.Second).UnixNano())
			if !bytes.Equal(got, data) {
				t.Fatalf("dirty bytes = %q, want %q", got, data)
			}
		})
	}
}

// A volume-allocation failure poisons a buffered handle. Model that failure
// directly: no Windows VM, volume server, or disk-filling loop is required.
func newUploadFailedHandle(t *testing.T) (*WFS, *FileHandle, fuse.InHeader) {
	t.Helper()
	wfs, _ := newCreateTestWFS(t)
	out := &fuse.CreateOut{}
	if status := wfs.Create(nil, &fuse.CreateIn{
		InHeader: fuse.InHeader{NodeId: 1}, Flags: syscall.O_RDWR | syscall.O_CREAT, Mode: 0o644,
	}, "upload-failed", out); status != fuse.OK {
		t.Fatalf("Create: %v", status)
	}
	fh := wfs.GetHandle(FileHandleId(out.Fh))
	t.Cleanup(fh.dirtyPages.Destroy)
	pages := fh.dirtyPages.randomWriter.(*ChunkedDirtyPages)
	pages.hasWrites = true
	pages.setLastError(errors.New("assign volume failure: no writable volumes"))
	return wfs, fh, fuse.InHeader{NodeId: out.NodeId}
}

func TestRejectedWriteDoesNotMutateEntry(t *testing.T) {
	wfs, fh, header := newUploadFailedHandle(t)
	entry := fh.GetEntry()
	entry.Content = []byte("intact")
	entry.Attributes.FileSize = uint64(len(entry.Content))
	entry.Attributes.Mtime, entry.Attributes.Ctime = 10, 11
	entry.Attributes.MtimeNs, entry.Attributes.CtimeNs = 12, 13
	before := proto.Clone(entry.GetEntry()).(*filer_pb.Entry)
	wasDirty := fh.dirtyMetadata
	wfs.option.Quota = 1 << 30
	uncommittedBefore := wfs.GetUncommittedBytes()
	t.Cleanup(func() { wfs.SubtractUncommittedBytes(wfs.GetUncommittedBytes() - uncommittedBefore) })
	data := []byte("rejected")
	n, status := wfs.Write(nil, &fuse.WriteIn{
		InHeader: header, Fh: uint64(fh.fh), Offset: 100, Size: uint32(len(data)),
	}, data)
	if n != 0 || status != fuse.Status(syscall.ENOSPC) {
		t.Fatalf("Write = (%d, %v), want (0, ENOSPC)", n, status)
	}
	if !proto.Equal(before, fh.GetEntry().GetEntry()) {
		t.Errorf("rejected Write changed entry: before=%v after=%v", before, fh.GetEntry().GetEntry())
	}
	if fh.dirtyMetadata != wasDirty {
		t.Errorf("rejected Write changed dirty metadata flag")
	}
	if got := wfs.GetUncommittedBytes(); got != uncommittedBefore {
		t.Errorf("rejected Write charged %d uncommitted bytes", got-uncommittedBefore)
	}
}

// WinFsp's explicit FlushFileBuffers calls Fsync, not its void Close callback.
// Even writeback mode must report this error on every explicit sync; treating
// the second attempt as clean would incorrectly acknowledge lost data.
func TestFsyncRetainsUploadFailure(t *testing.T) {
	for _, writeback := range []bool{false, true} {
		t.Run(map[bool]string{false: "synchronous", true: "writeback"}[writeback], func(t *testing.T) {
			wfs, fh, header := newUploadFailedHandle(t)
			wfs.option.WritebackCache = writeback
			for attempt := 0; attempt < 2; attempt++ {
				status := wfs.Fsync(nil, &fuse.FsyncIn{InHeader: header, Fh: uint64(fh.fh)})
				if status != fuse.Status(syscall.ENOSPC) {
					t.Fatalf("Fsync attempt %d = %v, want ENOSPC", attempt, status)
				}
				if !fh.dirtyMetadata {
					t.Fatal("failed Fsync incorrectly marked metadata clean")
				}
			}
		})
	}
}
