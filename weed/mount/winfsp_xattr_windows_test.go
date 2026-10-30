//go:build windows

package mount

import (
	"context"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// WinFsp turns NT extended attributes into these calls, so they must reach
// the shared xattr handlers that Linux mounts use.
func TestWinfspExtendedAttributesReadTheEntry(t *testing.T) {
	wfs := newPagingWFS(t, "/d", []string{"f"}, 0)
	now := time.Now()
	p := util.FullPath("/d/f")
	if err := wfs.metaCache.InsertEntry(context.Background(), &filer.Entry{
		FullPath: p,
		Attr:     filer.Attr{Mode: 0o644, Mtime: now, Crtime: now, FileSize: 1, Inode: p.AsInode(now.Unix())},
		Extended: map[string][]byte{"xattr-USER.TAG": []byte("v1"), "xattr-USER.OTHER": []byte("v2"), "s3-meta": []byte("x")},
	}, 0); err != nil {
		t.Fatal(err)
	}
	a := newWinfspFS(wfs, false)

	if status, value := a.Getxattr("/d/f", "USER.TAG"); status != 0 || string(value) != "v1" {
		t.Fatalf("Getxattr = %d, %q; want 0, %q", status, value, "v1")
	}
	var names []string
	if status := a.Listxattr("/d/f", func(name string) bool { names = append(names, name); return true }); status != 0 {
		t.Fatalf("Listxattr = %d", status)
	}
	if len(names) != 2 {
		t.Fatalf("Listxattr names = %q; want the two xattr- keys only", names)
	}
	if status, _ := a.Getxattr("/d/f", "USER.MISSING"); status == 0 {
		t.Fatal("Getxattr of a missing attribute succeeded")
	}
}
