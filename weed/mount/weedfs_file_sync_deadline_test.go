package mount

import (
	"syscall"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/mount/page_writer"
)

// slowFlushPages stands in for a handle whose chunk uploads take longer than
// the metadata deadline: a large checkpoint draining at link speed.
type slowFlushPages struct {
	page_writer.DirtyPages
	delay time.Duration
}

func (p *slowFlushPages) FlushData() error {
	time.Sleep(p.delay)
	return nil
}

// The metadata deadline bounds the CreateEntry commit, not the data upload
// before it. Starting it before FlushData made every close() whose uploads
// outlasted it fail with EIO after the data was already stored.
func TestFlushCommitsMetadataAfterUploadsOutlastTheMetadataDeadline(t *testing.T) {
	saved := metadataFlushTimeout
	metadataFlushTimeout = 200 * time.Millisecond
	defer func() { metadataFlushTimeout = saved }()

	wfs, testServer := newCreateTestWFS(t)
	out := &fuse.CreateOut{}
	if status := wfs.Create(make(chan struct{}), &fuse.CreateIn{
		InHeader: fuse.InHeader{NodeId: 1},
		Flags:    syscall.O_WRONLY | syscall.O_CREAT,
		Mode:     0o644,
	}, "checkpoint.pt", out); status != fuse.OK {
		t.Fatalf("Create status = %v, want OK", status)
	}
	fh := wfs.GetHandle(FileHandleId(out.Fh))
	fh.dirtyPages.randomWriter = &slowFlushPages{DirtyPages: fh.dirtyPages.randomWriter, delay: 3 * metadataFlushTimeout}

	for name, flush := range map[string]func() fuse.Status{
		"close": func() fuse.Status {
			return wfs.Flush(make(chan struct{}), &fuse.FlushIn{InHeader: fuse.InHeader{NodeId: out.NodeId}, Fh: out.Fh})
		},
		"fsync": func() fuse.Status {
			return wfs.Fsync(make(chan struct{}), &fuse.FsyncIn{InHeader: fuse.InHeader{NodeId: out.NodeId}, Fh: out.Fh})
		},
	} {
		fh.dirtyMetadata = true
		before := testServer.creates()
		if status := flush(); status != fuse.OK {
			t.Fatalf("%s after slow uploads = %v, want OK", name, status)
		}
		if testServer.creates() == before {
			t.Fatalf("%s did not commit the entry", name)
		}
	}
}
