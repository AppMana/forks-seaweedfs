//go:build windows

package mount

import (
	"context"
	"testing"
	"time"

	cgofuse "github.com/winfsp/cgofuse/fuse"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// WinFsp has no read-only option for FUSE file systems, so a -readOnly mount
// must refuse every mutation in the adapter.
func TestWinfspReadOnlyMountRefusesMutations(t *testing.T) {
	wfs := newPagingWFS(t, "/d", []string{"f"}, 0)
	now := time.Now()
	p := util.FullPath("/d/f")
	if err := wfs.metaCache.InsertEntry(context.Background(), &filer.Entry{
		FullPath: p,
		Attr:     filer.Attr{Mode: 0o644, Mtime: now, Crtime: now, FileSize: 1, Inode: p.AsInode(now.Unix())},
	}, 0); err != nil {
		t.Fatal(err)
	}
	a := newWinfspFS(wfs, false)
	a.readOnly = true

	create, _ := a.Create("/d/new", cgofuse.O_CREAT|cgofuse.O_RDWR, 0o644)
	mutations := map[string]int{
		"Create":      create,
		"Mkdir":       a.Mkdir("/d/dir", 0o755),
		"Mknod":       a.Mknod("/d/node", 0o644, 0),
		"Rmdir":       a.Rmdir("/d"),
		"Unlink":      a.Unlink("/d/f"),
		"Rename":      a.Rename("/d/f", "/d/g"),
		"Symlink":     a.Symlink("f", "/d/link"),
		"Link":        a.Link("/d/f", "/d/hard"),
		"Chmod":       a.Chmod("/d/f", 0o600),
		"Chown":       a.Chown("/d/f", 1, 1),
		"Utimens":     a.Utimens("/d/f", []cgofuse.Timespec{{}, {}}),
		"Truncate":    a.Truncate("/d/f", 0, invalidFh),
		"Write":       a.Write("/d/f", []byte("x"), 0, invalidFh),
		"Setxattr":    a.Setxattr("/d/f", "USER.TAG", []byte("v"), 0),
		"Removexattr": a.Removexattr("/d/f", "USER.TAG"),
		"Chflags":     a.Chflags("/d/f", 0),
		"Setcrtime":   a.Setcrtime("/d/f", cgofuse.Timespec{}),
	}
	for op, status := range mutations {
		if status != -cgofuse.EROFS {
			t.Errorf("%s = %d; want -EROFS (%d)", op, status, -cgofuse.EROFS)
		}
	}

	entry, _, err := wfs.metaCache.FindEntry(context.Background(), p)
	if err != nil || entry.Attr.Mode != 0o644 || entry.Attr.FileSize != 1 {
		t.Fatalf("entry changed on a read-only mount: %+v %v", entry, err)
	}
}
