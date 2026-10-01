package mount

import (
	"testing"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func TestRemoteDeletePathPreservesReplacementAndOtherLinks(t *testing.T) {
	table := NewInodeToPath("/", 0)
	path := util.FullPath("/file")
	old := table.Lookup(path, 1, false, false, 42, true)
	table.AddPath(old, "/other-link")
	table.RemovePathForInode(path, old)
	if table.HasPath(path) {
		t.Fatal("removed name remains reachable")
	}
	if got, status := table.GetPath(old); status != fuse.OK || got != "/other-link" {
		t.Fatalf("other link lost: %s %v", got, status)
	}
	replacement := table.Lookup(path, 2, false, false, 43, true)
	table.RemovePathForInode(path, old)
	if got, found := table.GetInode(path); !found || got != replacement {
		t.Fatal("delayed deletion removed the replacement")
	}
	table.RemovePathForInode("/other-link", old)
	if !table.HasInode(old) {
		t.Fatal("remote unlink released an inode still referenced by the kernel")
	}
	if _, status := table.GetPath(old); status != fuse.ENOENT {
		t.Fatal("unlinked inode still has a name")
	}
}
