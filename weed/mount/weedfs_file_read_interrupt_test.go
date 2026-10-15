package mount

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util/chunk_cache"
)

// Exercise the raw FUSE handler, including the interrupt channel and chunk
// reader, rather than only testing an errno conversion helper.
func TestReadKernelInterruptStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		interrupt bool
		backend   error
		want      fuse.Status
	}{
		{"kernel interrupt", true, nil, fuse.EINTR},
		{"backend cancellation without interrupt", false, context.Canceled, fuse.EIO},
		{"backend deadline without interrupt", false, context.DeadlineExceeded, fuse.EIO},
		{"backend failure", false, errors.New("backend unavailable"), fuse.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wfs, inode, fh := newUnlinkedOpenFile(t)
			started := make(chan struct{})
			cancel := make(chan struct{})
			release := make(chan struct{})
			chunks := []*filer_pb.FileChunk{{FileId: "1,0000000100000001", Size: 8, ModifiedTsNs: 1}}
			group, err := filer.NewChunkGroup(func(ctx context.Context, _ string) ([]string, error) {
				close(started)
				if tc.interrupt {
					// Shared chunk fetches outlive an individual FUSE request.
					// Release the fake backend during cleanup, after the read
					// has returned its interrupt status.
					select {
					case <-ctx.Done():
					case <-release:
					}
					return nil, context.Canceled
				}
				return nil, tc.backend
			}, (*chunk_cache.TieredChunkCache)(nil), chunks, 1, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = group.Close() })
			t.Cleanup(func() { close(release) })
			fh.entryChunkGroup = group
			fh.entry.Entry.Chunks = chunks
			fh.entry.Entry.Attributes.FileSize = 8
			in := &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: inode}, Fh: uint64(fh.fh), Size: 4}
			done := make(chan fuse.Status, 1)
			go func() {
				_, status := wfs.Read(cancel, in, make([]byte, 4))
				done <- status
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				close(cancel)
				t.Fatal("read did not reach the chunk lookup")
			}
			if tc.interrupt {
				close(cancel)
			}
			select {
			case status := <-done:
				if status != tc.want {
					t.Errorf("Read status = %v, want %v", status, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("read did not finish")
			}
			// A canceled request must not poison the handle for later reads.
			fh.entry.Entry.Content = []byte("recovery")
			buf := make([]byte, 4)
			if _, status := wfs.Read(nil, in, buf); status != fuse.OK || string(buf) != "reco" {
				t.Fatalf("subsequent read = %q, %v", buf, status)
			}
			// Successfully available bytes win a late interrupt; do not
			// introduce a spurious failure when no backend read is needed.
			lateInterrupt := make(chan struct{})
			close(lateInterrupt)
			if _, status := wfs.Read(lateInterrupt, in, buf); status != fuse.OK || string(buf) != "reco" {
				t.Fatalf("available read with interrupt = %q, %v", buf, status)
			}
		})
	}
}
